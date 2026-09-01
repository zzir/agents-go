package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// fakeS3 is an in-memory S3-alike: the API side takes signed PUT/DELETE, the
// public side serves anonymous GETs — the two halves the real feature uses.
type fakeS3 struct {
	objects map[string][]byte
	api     *httptest.Server
	public  *httptest.Server
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path-style: /<bucket>/<key...> — strip whichever bucket the test used.
		_, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch r.Method {
		case http.MethodPut:
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(r.Body)
			f.objects[key] = b.Bytes()
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(f.api.Close)
	f.public = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := f.objects[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.public.Close)
	return f
}

// storageSettings writes a complete attachment-storage section pointing at f.
func storageSettings(t *testing.T, s *store.SettingStore, f *fakeS3) {
	t.Helper()
	ctx := context.Background()
	for k, v := range map[string]string{
		settings.KeyS3Endpoint: f.api.URL, settings.KeyS3Bucket: "b", settings.KeyS3PathStyle: "true",
		settings.KeyS3AccessKeyID: "AK", settings.KeyS3SecretAccessKey: "SK",
		settings.KeyS3PublicBaseURL: f.public.URL,
	} {
		if err := s.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartBody(t *testing.T, field, name string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	return body, mw.FormDataContentType()
}

func attachmentRig(t *testing.T) (*gin.Engine, *store.SettingStore, *store.AttachmentStore, *fakeS3) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	settingStore := store.NewSettingStore(db)
	atts := store.NewAttachmentStore(db)
	h := NewAttachmentHandler(atts, settings.NewReader(settingStore), settingStore)
	e := newTestEngine()
	e.GET("/attachments/config", h.Config)
	e.POST("/attachments", h.Upload)
	e.PUT("/attachments/storage", h.SaveStorage)
	e.POST("/attachments/storage/test", h.TestStorage)
	e.DELETE("/attachments/:id", h.Delete)
	return e, settingStore, atts, newFakeS3(t)
}

func TestAttachmentUploadRoundTrip(t *testing.T) {
	e, settingStore, atts, s3 := attachmentRig(t)
	storageSettings(t, settingStore, s3)

	body, ctype := multipartBody(t, "file", "a.png", pngBytes(t, 12, 8))
	req := httptest.NewRequest(http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", w.Code, w.Body)
	}
	var resp attachmentResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Mime != "image/png" || resp.Size == 0 {
		t.Fatalf("resp = %+v", resp)
	}
	if !strings.HasPrefix(resp.URL, s3.public.URL+"/attachments/"+store.LocalUserID+"/") {
		t.Fatalf("url = %q", resp.URL)
	}
	// The bytes actually landed in the bucket, fetchable anonymously.
	pub, err := http.Get(resp.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Body.Close()
	if pub.StatusCode != http.StatusOK {
		t.Fatalf("public read = %d", pub.StatusCode)
	}

	// Delete of the unsent upload removes row and object.
	req = httptest.NewRequest(http.MethodDelete, "/attachments/"+resp.ID, nil)
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body)
	}
	if len(s3.objects) != 0 {
		t.Fatalf("object survived delete: %v", s3.objects)
	}
	if _, err := atts.Get(context.Background(), resp.ID); err == nil {
		t.Fatal("row survived delete")
	}
}

func TestAttachmentUploadRejections(t *testing.T) {
	e, settingStore, _, s3 := attachmentRig(t)

	// Unconfigured storage answers 503, not an opaque failure.
	body, ctype := multipartBody(t, "file", "a.png", pngBytes(t, 2, 2))
	req := httptest.NewRequest(http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured upload = %d", w.Code)
	}

	storageSettings(t, settingStore, s3)

	// A non-image (or unsupported format) is refused by decoding, whatever
	// the filename claims.
	body, ctype = multipartBody(t, "file", "a.png", []byte("GIF89a not really an image"))
	req = httptest.NewRequest(http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ctype)
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-image upload = %d: %s", w.Code, w.Body)
	}
	if len(s3.objects) != 0 {
		t.Fatalf("rejected upload left an object: %v", s3.objects)
	}
}

func TestAttachmentDeleteGates(t *testing.T) {
	e, settingStore, atts, s3 := attachmentRig(t)
	storageSettings(t, settingStore, s3)
	ctx := context.Background()

	// A bound attachment is session history: 409.
	bound := &store.Attachment{OwnerID: store.LocalUserID, Key: "att/b.png", Mime: "image/png", Size: 1, Bound: true}
	if err := atts.Create(ctx, bound); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/attachments/"+bound.ID, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete bound = %d", w.Code)
	}

	// Someone else's attachment reads as absent.
	foreign := &store.Attachment{OwnerID: store.NewID(), Key: "att/f.png", Mime: "image/png", Size: 1}
	if err := atts.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/attachments/"+foreign.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete foreign = %d", w.Code)
	}
}

func TestAttachmentConfigEndpoint(t *testing.T) {
	e, settingStore, _, s3 := attachmentRig(t)

	var cfg attachmentConfigResp
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/attachments/config", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.MaxCount != 8 || cfg.DownscalePx != 1568 {
		t.Fatalf("unconfigured cfg = %+v", cfg)
	}

	storageSettings(t, settingStore, s3)
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/attachments/config", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled {
		t.Fatal("configured storage reports disabled")
	}
}

// The storage form: the section saves, tests and clears as ONE group. The
// regression this shape exists for: with a section already stored, changing
// one field (the bucket) must validate the NEW section — per-key saves
// validated the new value against the old siblings and refused the change.
func TestStorageFormLifecycle(t *testing.T) {
	e, settingStore, _, s3 := attachmentRig(t)
	reader := settings.NewReader(settingStore)
	ctx := context.Background()

	body := func(endpoint, bucket, secret, pub string) *bytes.Reader {
		b, _ := json.Marshal(map[string]any{
			"endpoint": endpoint, "bucket": bucket, "path_style": true,
			"access_key_id": "AK", "secret_access_key": secret, "public_base_url": pub,
		})
		return bytes.NewReader(b)
	}
	call := func(method, path string, r *bytes.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w
	}

	// Save a valid section.
	if w := call(http.MethodPut, "/attachments/storage", body(s3.api.URL, "b", "SK", s3.public.URL)); w.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", w.Code, w.Body)
	}
	if cfg := reader.S3Config(ctx); !cfg.Complete() || cfg.Bucket != "b" || !cfg.PathStyle {
		t.Fatalf("stored config = %+v", cfg)
	}

	// THE regression: change the bucket with everything else stored — the
	// save must probe the new section as a whole and succeed.
	if w := call(http.MethodPut, "/attachments/storage", body(s3.api.URL, "b2", SecretMask, s3.public.URL)); w.Code != http.StatusOK {
		t.Fatalf("bucket change refused: %d %s", w.Code, w.Body)
	}
	if cfg := reader.S3Config(ctx); cfg.Bucket != "b2" || cfg.SecretKey != "SK" {
		t.Fatalf("after bucket change: %+v (masked secret must keep the stored one)", cfg)
	}

	// A bad public base is refused with the probe's diagnosis, and nothing
	// is stored.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer dead.Close()
	if w := call(http.MethodPut, "/attachments/storage", body(s3.api.URL, "b3", SecretMask, dead.URL)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "anonymous reads") {
		t.Fatalf("bad public base: %d %s", w.Code, w.Body)
	}
	if cfg := reader.S3Config(ctx); cfg.Bucket != "b2" {
		t.Fatalf("refused save leaked into storage: %+v", cfg)
	}

	// Test probes without storing.
	if w := call(http.MethodPost, "/attachments/storage/test", body(s3.api.URL, "b9", SecretMask, s3.public.URL)); w.Code != http.StatusNoContent {
		t.Fatalf("test = %d: %s", w.Code, w.Body)
	}
	if cfg := reader.S3Config(ctx); cfg.Bucket != "b2" {
		t.Fatalf("test stored something: %+v", cfg)
	}
	if len(s3.objects) != 0 {
		t.Fatalf("probes left objects: %v", s3.objects)
	}

	// A partial section is refused outright.
	if w := call(http.MethodPut, "/attachments/storage", body(s3.api.URL, "", SecretMask, s3.public.URL)); w.Code != http.StatusBadRequest {
		t.Fatalf("partial save = %d", w.Code)
	}

	// Clear: an all-empty body removes the section and turns the feature off.
	empty, _ := json.Marshal(map[string]any{})
	if w := call(http.MethodPut, "/attachments/storage", bytes.NewReader(empty)); w.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", w.Code, w.Body)
	}
	if cfg := reader.S3Config(ctx); cfg.Endpoint != "" || cfg.SecretKey != "" {
		t.Fatalf("clear left values: %+v", cfg)
	}
}

// Per-key writes of storage keys are refused: the section's keys are only
// valid together, and the per-key path validating against old siblings is
// the bug the form replaced.
func TestStorageKeysRefusePerKeyWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	settingStore := store.NewSettingStore(db)
	h := NewSettingHandler(settingStore)
	e := newTestEngine()
	e.PUT("/settings/:key", h.Set)
	e.DELETE("/settings/:key", h.Delete)

	b, _ := json.Marshal(map[string]string{"value": "x"})
	req := httptest.NewRequest(http.MethodPut, "/settings/"+settings.KeyS3Bucket, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Attachment storage form") {
		t.Fatalf("per-key put = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/settings/"+settings.KeyS3SecretAccessKey, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("per-key delete = %d %s", w.Code, w.Body)
	}
}
