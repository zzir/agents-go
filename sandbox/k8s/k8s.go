// Package k8s implements the sandbox.Sandbox interface using Kubernetes Jobs.
// Each Exec creates a ConfigMap with the request files and a locked-down Job
// that runs the command to completion, then collects the pod logs and exit code
// and deletes the objects.
//
// It pulls client-go and is a separate module so the core agents-go module stays
// dependency-light. True network isolation requires a deny-all NetworkPolicy or
// a sandboxed runtimeClass in the target namespace; the Job's securityContext
// covers the rest (non-root, read-only root fs, dropped capabilities, no service
// account token, resource and deadline limits).
package k8s

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/zzir/agents-go/sandbox"
)

// Options configures the Kubernetes sandbox.
type Options struct {
	// Image is the container image to run. Required.
	Image string
	// Namespace to create Jobs in. Defaults to "default".
	Namespace string
	// Limits caps the container's resources.
	Limits sandbox.Limits
	// RunAsUser sets the UID the process runs as. Defaults to 65534 (nobody).
	RunAsUser int64
	// TTLSecondsAfterFinished auto-deletes finished Jobs. Defaults to 300.
	TTLSecondsAfterFinished int32
	// RestConfig overrides client configuration. When nil, in-cluster config is
	// used, falling back to the default kubeconfig.
	RestConfig *rest.Config
	// PollInterval is how often Job status is polled. Defaults to 500ms.
	PollInterval time.Duration
	// StartTimeout bounds pod scheduling and image pull: the request's exec
	// timeout only starts counting once the container is actually running, so
	// a cold image pull is not misreported as an execution timeout. Defaults
	// to 2 minutes.
	StartTimeout time.Duration
}

func (o Options) namespace() string {
	if o.Namespace == "" {
		return "default"
	}
	return o.Namespace
}

func (o Options) startTimeout() time.Duration {
	if o.StartTimeout <= 0 {
		return 2 * time.Minute
	}
	return o.StartTimeout
}

// Sandbox is a Kubernetes-backed sandbox.Sandbox.
type Sandbox struct {
	cli  kubernetes.Interface
	opts Options
}

// New builds a Sandbox. Configuration is taken from opts.RestConfig, else from
// in-cluster config, else from the default kubeconfig.
func New(opts Options) (*Sandbox, error) {
	if opts.Image == "" {
		return nil, fmt.Errorf("k8s sandbox: Image is required")
	}
	cfg := opts.RestConfig
	if cfg == nil {
		var err error
		if cfg, err = rest.InClusterConfig(); err != nil {
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return nil, fmt.Errorf("k8s sandbox: load config: %w", err)
			}
		}
	}
	normalizeLoopbackHost(cfg)
	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s sandbox: client: %w", err)
	}
	return NewWithClient(cli, opts), nil
}

// normalizeLoopbackHost rewrites a 0.0.0.0 API-server host to 127.0.0.1. Local
// clusters (notably k3d) write the server address as https://0.0.0.0:<port>,
// which is not a valid connect target on macOS and fails with "unexpected EOF".
// A real remote server is never 0.0.0.0, so this rewrite is safe.
func normalizeLoopbackHost(cfg *rest.Config) {
	if cfg == nil {
		return
	}
	cfg.Host = strings.Replace(cfg.Host, "//0.0.0.0:", "//127.0.0.1:", 1)
}

// NewWithClient builds a Sandbox from an existing client (useful for testing
// with a fake clientset).
func NewWithClient(cli kubernetes.Interface, opts Options) *Sandbox {
	if opts.RunAsUser == 0 {
		opts.RunAsUser = 65534
	}
	if opts.TTLSecondsAfterFinished == 0 {
		opts.TTLSecondsAfterFinished = 300
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	return &Sandbox{cli: cli, opts: opts}
}

// Exec implements sandbox.Sandbox. Note: the Job's combined stdout+stderr is
// returned in ExecResult.Stdout, capped at req.MaxOutputBytes (Kubernetes pod
// logs do not separate the streams). ExecRequest.Stdin is not supported.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	if req.Stdin != "" {
		return nil, fmt.Errorf("k8s sandbox: ExecRequest.Stdin is not supported")
	}
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("k8s sandbox: ExecRequest.Cmd is empty")
	}
	ns := s.opts.namespace()

	cmSpec, filePaths, err := buildConfigMap(req.Files)
	if err != nil {
		return nil, err
	}
	cm, err := s.cli.CoreV1().ConfigMaps(ns).Create(ctx, cmSpec, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s sandbox: create configmap: %w", err)
	}
	defer s.cli.CoreV1().ConfigMaps(ns).Delete(context.WithoutCancel(ctx), cm.Name, metav1.DeleteOptions{})

	job, err := s.cli.BatchV1().Jobs(ns).Create(ctx, buildJob(cm.Name, filePaths, req, s.opts), metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s sandbox: create job: %w", err)
	}
	bg := metav1.DeletePropagationBackground
	defer s.cli.BatchV1().Jobs(ns).Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})

	// Make the Job own the ConfigMap: if this process dies before the deferred
	// delete runs, the ConfigMap is garbage-collected together with the Job
	// (which has a TTL) instead of leaking forever. Best effort.
	cm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}
	_, _ = s.cli.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})

	return s.waitAndCollect(ctx, ns, job.Name, req)
}

// collectTimeout bounds reading the pod state and logs after a timeout, when
// the polling context may already be spent.
const collectTimeout = 10 * time.Second

// waitAndCollect waits for the Job in two phases — container startup (bounded
// by Options.StartTimeout) and execution (bounded by the request timeout) —
// then reads the pod's exit code and logs.
func (s *Sandbox) waitAndCollect(ctx context.Context, ns, jobName string, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()

	// Phase 1: startup. Scheduling and image pulls must not eat into the
	// request's execution timeout, and fatal image errors surface as errors
	// instead of bogus timeouts.
	startDeadline := time.Now().Add(s.opts.startTimeout())
	jobDone, timedOut, started := false, false, false
	for !started && !jobDone {
		job, err := s.cli.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("k8s sandbox: get job: %w", err)
		}
		if jobDone, timedOut = jobStatus(job); jobDone {
			break
		}
		pod, err := s.findPod(ctx, ns, jobName)
		if err != nil {
			return nil, err
		}
		var fatal string
		if started, fatal = podStarted(pod); fatal != "" {
			return nil, fmt.Errorf("k8s sandbox: container cannot start: %s", fatal)
		}
		if started {
			break
		}
		if time.Now().After(startDeadline) {
			return nil, fmt.Errorf("k8s sandbox: container did not start within %v", s.opts.startTimeout())
		}
		select {
		case <-ctx.Done():
			// Caller cancellation is not an execution timeout.
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}

	// Phase 2: execution. The request timeout starts only now that the
	// container is running.
	execDeadline := time.Now().Add(req.EffectiveTimeout())
	for !jobDone {
		job, err := s.cli.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("k8s sandbox: get job: %w", err)
		}
		if jobDone, timedOut = jobStatus(job); jobDone {
			break
		}
		if time.Now().After(execDeadline) {
			timedOut = true
			break
		}
		select {
		case <-ctx.Done():
			// Caller cancellation is not an execution timeout.
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}

	res := &sandbox.ExecResult{TimedOut: timedOut}

	// Collect the exit code and logs. After a timeout do so on a fresh,
	// briefly-bounded context: an already-expired deadline must not turn the
	// timeout result into an error.
	cctx := ctx
	if timedOut {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), collectTimeout)
		defer cancel()
	}
	pod, err := s.findPod(cctx, ns, jobName)
	if err != nil || pod == nil {
		if timedOut {
			// The pod may already be gone after the deadline; there is no exit
			// code to report.
			res.ExitCode = -1
			return res, nil
		}
		return res, err
	}
	if ec, ok := terminatedExitCode(pod); ok {
		res.ExitCode = ec
	} else if timedOut {
		res.ExitCode = -1
	}
	logs, err := s.podLogs(cctx, ns, pod.Name, req.EffectiveMaxOutputBytes())
	if err != nil {
		if timedOut {
			// Best effort after a timeout: report the timeout without logs.
			return res, nil
		}
		return nil, fmt.Errorf("k8s sandbox: pod logs: %w", err)
	}
	res.Stdout = logs
	return res, nil
}

func (s *Sandbox) findPod(ctx context.Context, ns, jobName string) (*corev1.Pod, error) {
	pods, err := s.cli.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return nil, fmt.Errorf("k8s sandbox: list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

func (s *Sandbox) podLogs(ctx context.Context, ns, podName string, maxBytes int64) (string, error) {
	raw, err := s.cli.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{LimitBytes: &maxBytes}).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Close implements sandbox.Sandbox. The clientset needs no teardown.
func (s *Sandbox) Close() error { return nil }

var _ sandbox.Sandbox = (*Sandbox)(nil)

// --- pure builders (unit-tested without a cluster) ---

const workMount = "/work"

// activeDeadlineGraceSeconds pads the Job's server-side deadline backstop.
const activeDeadlineGraceSeconds = 10

func ptr[T any](v T) *T { return &v }

// buildConfigMap packs files into a ConfigMap using indexed keys ("f0", "f1",
// ...), because ConfigMap keys cannot contain "/". The returned slice holds
// the cleaned relative file paths in sorted order; paths[i] is stored under
// key "f<i>". Paths escaping the working directory are rejected.
func buildConfigMap(files map[string]string) (*corev1.ConfigMap, []string, error) {
	paths := make([]string, 0, len(files))
	byPath := make(map[string]string, len(files))
	for name, content := range files {
		clean, err := cleanRelPath(name)
		if err != nil {
			return nil, nil, err
		}
		if _, dup := byPath[clean]; dup {
			return nil, nil, fmt.Errorf("k8s sandbox: duplicate file path %q", clean)
		}
		byPath[clean] = content
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	data := make(map[string]string, len(paths))
	for i, p := range paths {
		data["f"+strconv.Itoa(i)] = byPath[p]
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "agents-sb-"},
		Data:       data,
	}, paths, nil
}

// cleanRelPath normalizes a request file path and rejects paths that would
// escape the working directory.
func cleanRelPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("k8s sandbox: empty file path")
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("k8s sandbox: file path %q must be relative to the working directory", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("k8s sandbox: file path %q escapes the working directory", name)
	}
	return clean, nil
}

func buildJob(configMapName string, filePaths []string, req sandbox.ExecRequest, opts Options) *batchv1.Job {
	// The execution timeout is enforced client-side from the moment the
	// container runs; ActiveDeadlineSeconds is only a server-side backstop, so
	// it budgets startup (scheduling, image pull) plus execution plus grace.
	deadline := int64((opts.startTimeout()+req.EffectiveTimeout())/time.Second) + activeDeadlineGraceSeconds

	limits := corev1.ResourceList{}
	if opts.Limits.MemoryBytes > 0 {
		limits[corev1.ResourceMemory] = *resource.NewQuantity(opts.Limits.MemoryBytes, resource.BinarySI)
	}
	if opts.Limits.CPUs > 0 {
		limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(opts.Limits.CPUs*1000), resource.DecimalSI)
	}

	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "work", MountPath: workMount},
	}
	if len(filePaths) > 0 {
		// Each request file is projected from the ConfigMap into the working
		// directory individually (subPath mounts keyed "f<i>"): nested paths
		// and dotfiles work, and no copy step (no shell) is needed in the
		// image. ConfigMap keys cannot contain "/", hence the indexed keys.
		volumes = append(volumes, corev1.Volume{Name: "code", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMapName}},
		}})
		for i, p := range filePaths {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      "code",
				MountPath: path.Join(workMount, p),
				SubPath:   "f" + strconv.Itoa(i),
				ReadOnly:  true,
			})
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "agents-sb-"},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(deadline),
			TTLSecondsAfterFinished: ptr(opts.TTLSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					Volumes:                      volumes,
					Containers: []corev1.Container{{
						Name:         "run",
						Image:        opts.Image,
						Command:      req.Cmd,
						WorkingDir:   workMount,
						Env:          envVars(req.Env),
						VolumeMounts: mounts,
						Resources:    corev1.ResourceRequirements{Limits: limits},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             ptr(true),
							RunAsUser:                ptr(opts.RunAsUser),
							ReadOnlyRootFilesystem:   ptr(true),
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
}

func envVars(env map[string]string) []corev1.EnvVar {
	if len(env) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

// jobStatus reports whether the job finished and whether it hit its deadline.
func jobStatus(job *batchv1.Job) (done, timedOut bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false
		case batchv1.JobFailed:
			return true, strings.Contains(c.Reason, "DeadlineExceeded")
		}
	}
	return false, false
}

// fatalImageReasons are container waiting reasons that will not resolve by
// waiting: the image cannot be pulled (or named), so they surface as errors
// instead of being misreported as timeouts.
var fatalImageReasons = map[string]bool{
	"ErrImagePull":     true,
	"ImagePullBackOff": true,
	"InvalidImageName": true,
}

// podStarted reports whether the pod's container is running (or already
// finished). A non-empty fatal describes a startup failure that will not
// resolve on its own.
func podStarted(pod *corev1.Pod) (started bool, fatal string) {
	if pod == nil {
		return false, ""
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return true, ""
		}
		if w := cs.State.Waiting; w != nil && fatalImageReasons[w.Reason] {
			if w.Message != "" {
				return false, w.Reason + ": " + w.Message
			}
			return false, w.Reason
		}
	}
	return false, ""
}

func terminatedExitCode(pod *corev1.Pod) (int, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return int(cs.State.Terminated.ExitCode), true
		}
	}
	return 0, false
}
