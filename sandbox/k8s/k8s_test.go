package k8s

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/zzir/agents-go/sandbox"
)

func TestBuildJob_SecurityAndLimits(t *testing.T) {
	opts := Options{
		Image:     "python:3.12-slim",
		RunAsUser: 65534,
		Limits:    sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5},
	}
	job := buildJob("cm-1", nil, sandbox.ExecRequest{Cmd: []string{"python", "main.py"}, Timeout: 10 * time.Second}, opts)

	spec := job.Spec
	if spec.BackoffLimit == nil || *spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", spec.BackoffLimit)
	}
	// The server-side backstop budgets startup (default 2m) + exec (10s) +
	// grace (10s); the exec timeout itself is enforced client-side.
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 140 {
		t.Errorf("activeDeadlineSeconds = %v, want 140", spec.ActiveDeadlineSeconds)
	}
	pod := spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("service account token should not be auto-mounted")
	}
	// No shell wrapper: the request command runs as-is.
	if !reflect.DeepEqual(pod.Containers[0].Command, []string{"python", "main.py"}) {
		t.Errorf("command = %v", pod.Containers[0].Command)
	}
	sc := pod.Containers[0].SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot should be true")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities drop = %v", sc.Capabilities.Drop)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp profile should be RuntimeDefault")
	}
	limits := pod.Containers[0].Resources.Limits
	if limits.Memory().Value() != 256<<20 {
		t.Errorf("memory limit = %v", limits.Memory())
	}
	if limits.Cpu().MilliValue() != 500 {
		t.Errorf("cpu limit = %v milli, want 500", limits.Cpu().MilliValue())
	}
}

func TestBuildJob_StartTimeoutExtendsBackstop(t *testing.T) {
	opts := Options{Image: "img", StartTimeout: 30 * time.Second}
	job := buildJob("cm-1", nil, sandbox.ExecRequest{Cmd: []string{"x"}, Timeout: 5 * time.Second}, opts)
	if got := *job.Spec.ActiveDeadlineSeconds; got != 45 {
		t.Errorf("activeDeadlineSeconds = %d, want 30+5+10 = 45", got)
	}
}

func TestBuildJob_FileSubPathMounts(t *testing.T) {
	_, paths, err := buildConfigMap(map[string]string{
		"main.py":     "print(1)",
		".env":        "A=1",
		"pkg/util.py": "u",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := buildJob("cm-1", paths, sandbox.ExecRequest{Cmd: []string{"python", "main.py"}}, Options{Image: "img"})

	pod := job.Spec.Template.Spec
	// Nested paths and dotfiles are mounted individually via subPath keys; no
	// copy script runs in the container.
	wantMounts := []corev1.VolumeMount{
		{Name: "work", MountPath: "/work"},
		{Name: "code", MountPath: "/work/.env", SubPath: "f0", ReadOnly: true},
		{Name: "code", MountPath: "/work/main.py", SubPath: "f1", ReadOnly: true},
		{Name: "code", MountPath: "/work/pkg/util.py", SubPath: "f2", ReadOnly: true},
	}
	if got := pod.Containers[0].VolumeMounts; !reflect.DeepEqual(got, wantMounts) {
		t.Errorf("volume mounts = %v, want %v", got, wantMounts)
	}
	var codeVol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "code" {
			codeVol = &pod.Volumes[i]
		}
	}
	if codeVol == nil || codeVol.ConfigMap == nil || codeVol.ConfigMap.Name != "cm-1" {
		t.Errorf("code volume should reference the config map, got %v", pod.Volumes)
	}
}

func TestBuildJob_NoFilesNoCodeVolume(t *testing.T) {
	job := buildJob("cm-1", nil, sandbox.ExecRequest{Cmd: []string{"x"}}, Options{Image: "img"})
	pod := job.Spec.Template.Spec
	if len(pod.Volumes) != 1 || pod.Volumes[0].Name != "work" {
		t.Errorf("expected only the work volume, got %v", pod.Volumes)
	}
}

func TestBuildConfigMap(t *testing.T) {
	cm, paths, err := buildConfigMap(map[string]string{
		"main.py":     "print(1)",
		".env":        "A=1",
		"pkg/util.py": "u",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cm.GenerateName == "" {
		t.Error("config map should use GenerateName")
	}
	if want := []string{".env", "main.py", "pkg/util.py"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	// Keys are indexed ("/" is not allowed in ConfigMap keys); paths[i] maps
	// to key f<i>.
	wantData := map[string]string{"f0": "A=1", "f1": "print(1)", "f2": "u"}
	if !reflect.DeepEqual(cm.Data, wantData) {
		t.Errorf("data = %v, want %v", cm.Data, wantData)
	}
}

func TestBuildConfigMap_RejectsEscapingPaths(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../evil.py", "a/../../b.py", "/abs.py"} {
		if _, _, err := buildConfigMap(map[string]string{bad: "x"}); err == nil {
			t.Errorf("path %q should be rejected", bad)
		}
	}
	// Lexically messy but safe paths are normalized, not rejected.
	_, paths, err := buildConfigMap(map[string]string{"a/./b.py": "x"})
	if err != nil || !reflect.DeepEqual(paths, []string{"a/b.py"}) {
		t.Errorf("paths = %v, err = %v", paths, err)
	}
}

func TestPodStarted(t *testing.T) {
	if started, _ := podStarted(nil); started {
		t.Error("nil pod must not count as started")
	}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if started, fatal := podStarted(pending); started || fatal != "" {
		t.Errorf("pending pod: started = %v, fatal = %q", started, fatal)
	}
	running := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	if started, _ := podStarted(running); !started {
		t.Error("running container must count as started")
	}
	terminated := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		},
	}}
	if started, _ := podStarted(terminated); !started {
		t.Error("terminated container must count as started")
	}
	pulling := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
		},
	}}
	if started, fatal := podStarted(pulling); started || fatal != "" {
		t.Errorf("ContainerCreating: started = %v, fatal = %q (must keep waiting)", started, fatal)
	}
	backoff := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}},
		},
	}}
	if _, fatal := podStarted(backoff); !strings.Contains(fatal, "ImagePullBackOff") {
		t.Errorf("fatal = %q, want ImagePullBackOff surfaced", fatal)
	}
}

// injectPodWhenJobExists creates a pod labeled for the first job that appears
// in ns, simulating the job controller + kubelet.
func injectPodWhenJobExists(cli *fake.Clientset, ns string, status corev1.PodStatus) {
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := cli.BatchV1().Jobs(ns).List(context.Background(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "agents-sb-pod",
						Namespace: ns,
						Labels:    map[string]string{"job-name": jobs.Items[0].Name},
					},
					Status: status,
				}
				_, _ = cli.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

func TestExec_CreatesAndCleansUpObjects(t *testing.T) {
	// The fake clientset never schedules a pod, so Exec fails in the startup
	// phase; this verifies object creation, the Job->ConfigMap owner reference
	// and the deferred cleanup.
	cli := fake.NewClientset()
	// The real API server resolves GenerateName into a concrete name; the fake
	// object tracker does not, so emulate it — the owner reference below is
	// built from the returned job's name.
	gen := 0
	cli.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if obj, ok := action.(k8stesting.CreateAction).GetObject().(metav1.Object); ok {
			if obj.GetName() == "" && obj.GetGenerateName() != "" {
				gen++
				obj.SetName(fmt.Sprintf("%s%d", obj.GetGenerateName(), gen))
			}
		}
		return false, nil, nil
	})
	var owners []metav1.OwnerReference
	cli.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		owners = cm.GetOwnerReferences()
		return false, nil, nil
	})
	sb := NewWithClient(cli, Options{Image: "busybox", Namespace: "sandbox",
		PollInterval: 2 * time.Millisecond, StartTimeout: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"echo", "hi"}, Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("err = %v, want a startup failure (the fake never runs pods)", err)
	}

	// The ConfigMap must be owned by the Job so a crash cannot leak it.
	if len(owners) != 1 || owners[0].Kind != "Job" || owners[0].Name == "" {
		t.Errorf("config map owner = %v, want the job", owners)
	}

	// The Job and ConfigMap should have been deleted by the deferred cleanup.
	jobs, _ := cli.BatchV1().Jobs("sandbox").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Errorf("jobs not cleaned up: %d", len(jobs.Items))
	}
	cms, _ := cli.CoreV1().ConfigMaps("sandbox").List(context.Background(), metav1.ListOptions{})
	if len(cms.Items) != 0 {
		t.Errorf("config maps not cleaned up: %d", len(cms.Items))
	}
}

func TestExec_TimeoutAfterStart(t *testing.T) {
	cli := fake.NewClientset()
	sb := NewWithClient(cli, Options{Image: "busybox", Namespace: "sandbox",
		PollInterval: 2 * time.Millisecond, StartTimeout: 3 * time.Second})
	injectPodWhenJobExists(cli, "sandbox", corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sleep", "10"}, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut with the pod still running past the exec deadline")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1 (no terminated state)", res.ExitCode)
	}
}

func TestExec_ReportsExitCodeFromPod(t *testing.T) {
	cli := fake.NewClientset()
	sb := NewWithClient(cli, Options{Image: "busybox", Namespace: "sandbox",
		PollInterval: 2 * time.Millisecond, StartTimeout: 3 * time.Second})
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			jobs, err := cli.BatchV1().Jobs("sandbox").List(context.Background(), metav1.ListOptions{})
			if err == nil && len(jobs.Items) > 0 {
				job := jobs.Items[0].DeepCopy()
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "agents-sb-pod", Namespace: "sandbox",
						Labels: map[string]string{"job-name": job.Name}},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
						ContainerStatuses: []corev1.ContainerStatus{
							{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 7}}},
						},
					},
				}
				_, _ = cli.CoreV1().Pods("sandbox").Create(context.Background(), pod, metav1.CreateOptions{})
				job.Status.Conditions = []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				}
				_, _ = cli.BatchV1().Jobs("sandbox").Update(context.Background(), job, metav1.UpdateOptions{})
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"false"}, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.TimedOut {
		t.Error("a BackoffLimitExceeded failure is not a timeout")
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res.ExitCode)
	}
}

func TestExec_ImagePullFailureIsError(t *testing.T) {
	cli := fake.NewClientset()
	sb := NewWithClient(cli, Options{Image: "no/such:image", Namespace: "sandbox",
		PollInterval: 2 * time.Millisecond, StartTimeout: 3 * time.Second})
	injectPodWhenJobExists(cli, "sandbox", corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "Back-off pulling image",
			}}},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("err = %v, want an ImagePullBackOff error, not a timeout", err)
	}
}

func TestExec_StdinRejected(t *testing.T) {
	sb := NewWithClient(fake.NewClientset(), Options{Image: "busybox"})
	_, err := sb.Exec(context.Background(), sandbox.ExecRequest{Cmd: []string{"cat"}, Stdin: "hi"})
	if err == nil || !strings.Contains(err.Error(), "Stdin is not supported") {
		t.Fatalf("err = %v, want a clear stdin rejection", err)
	}
}

func TestExec_CallerCancelDistinctFromTimeout(t *testing.T) {
	cli := fake.NewClientset()
	sb := NewWithClient(cli, Options{Image: "busybox", Namespace: "sandbox",
		PollInterval: 2 * time.Millisecond, StartTimeout: 10 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sleep", "5"}, Timeout: 10 * time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (not a TimedOut result)", err)
	}
}
