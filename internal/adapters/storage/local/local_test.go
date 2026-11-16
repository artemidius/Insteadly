package local

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func sampleOrder() domain.Order {
	return domain.Order{
		Clinic:  domain.Clinic{Name: "Praxis Test", Phone: "+431234567"},
		Patient: domain.Patient{FullName: "Max Mustermann", Phone: "+436641234567"},
	}
}

func mustCreate(t *testing.T, s *Store, o domain.Order) domain.Order {
	t.Helper()
	got, err := s.Orders().Create(context.Background(), o)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return got
}

func TestNewCreatesLayout(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, sub := range []string{"orders", "blobs"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Fatalf("stat %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", sub)
		}
	}
	if _, err := New(""); err == nil {
		t.Error("New(\"\") should fail")
	}
}

func TestCreateAssignsIDAndTimestamps(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())

	if !validOrderID(o.ID) {
		t.Errorf("id %q is not 32-hex", o.ID)
	}
	if o.Status != domain.OrderCreated {
		t.Errorf("status = %q, want created", o.Status)
	}
	if o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", o)
	}

	path := filepath.Join(s.dir, "orders", string(o.ID)+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("order file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("order file perm = %o, want 600", perm)
	}

	got, err := s.Orders().Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Clinic.Name != "Praxis Test" || got.Patient.FullName != "Max Mustermann" {
		t.Errorf("roundtrip lost data: %+v", got)
	}
}

func TestCreateForcesCreatedStatus(t *testing.T) {
	s := newStore(t)
	o := sampleOrder()
	o.Status = domain.OrderPaid
	got := mustCreate(t, s, o)
	if got.Status != domain.OrderCreated {
		t.Errorf("status = %q, want created", got.Status)
	}
}

func TestCreateConflict(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())
	_, err := s.Orders().Create(context.Background(), domain.Order{ID: o.ID})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCreateInvalidID(t *testing.T) {
	s := newStore(t)
	_, err := s.Orders().Create(context.Background(), domain.Order{ID: "../../etc/passwd"})
	if err == nil {
		t.Fatal("expected error for invalid id")
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)
	_, err := s.Orders().Get(context.Background(), domain.NewOrderID())
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdatePersists(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())

	got, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
		order.Attempts = 3
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Attempts != 3 {
		t.Errorf("returned attempts = %d, want 3", got.Attempts)
	}
	stored, err := s.Orders().Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Attempts != 3 {
		t.Errorf("stored attempts = %d, want 3", stored.Attempts)
	}
}

func TestUpdateMissing(t *testing.T) {
	s := newStore(t)
	_, err := s.Orders().Update(context.Background(), domain.NewOrderID(), func(*domain.Order) error { return nil })
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateChangingIDRejected(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())
	_, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
		order.ID = domain.NewOrderID()
		return nil
	})
	if err == nil {
		t.Fatal("expected error when fn changes id")
	}
}

func TestUpdateFnErrorLeavesOrderUntouched(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())
	sentinel := errors.New("nope")
	_, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
		order.Attempts = 99
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want fn error", err)
	}
	stored, err := s.Orders().Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Attempts != 0 {
		t.Errorf("stored attempts = %d, want 0", stored.Attempts)
	}
}

func TestUpdateNilFn(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())
	if _, err := s.Orders().Update(context.Background(), o.ID, nil); err == nil {
		t.Fatal("expected error for nil fn")
	}
}

func TestListNewestFirst(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	ids := make([]domain.OrderID, 0, 3)
	for i := range 3 {
		o := sampleOrder()
		o.CreatedAt = base.Add(time.Duration(i) * time.Hour)
		created := mustCreate(t, s, o)
		ids = append(ids, created.ID)
	}

	list, err := s.Orders().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	for i, want := range []domain.OrderID{ids[2], ids[1], ids[0]} {
		if list[i].ID != want {
			t.Errorf("list[%d].ID = %s, want %s", i, list[i].ID, want)
		}
	}
}

func TestListEmptyIsNonNil(t *testing.T) {
	s := newStore(t)
	list, err := s.Orders().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list == nil {
		t.Fatal("List returned nil, want empty slice")
	}
	if len(list) != 0 {
		t.Fatalf("len = %d, want 0", len(list))
	}
}

func TestListCorruptFileIsError(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, sampleOrder())
	if err := os.WriteFile(filepath.Join(s.dir, "orders", "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}
	if _, err := s.Orders().List(context.Background()); err == nil {
		t.Fatal("expected error for corrupt order file")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	s := newStore(t)
	o := mustCreate(t, s, sampleOrder())

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Orders().Update(context.Background(), o.ID, func(order *domain.Order) error {
				order.Attempts++
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update: %v", err)
		}
	}

	stored, err := s.Orders().Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Attempts != workers {
		t.Errorf("attempts = %d, want %d", stored.Attempts, workers)
	}
}

func TestBlobRoundtrip(t *testing.T) {
	s := newStore(t)
	key := domain.BlobKey{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "call.wav"}
	want := []byte("canonical pcm bytes")

	if err := s.Blobs().Put(context.Background(), key, bytes.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Blobs().Open(context.Background(), key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got := new(bytes.Buffer)
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("blob = %q, want %q", got.Bytes(), want)
	}
}

func TestBlobSystemWide(t *testing.T) {
	s := newStore(t)
	key := domain.BlobKey{Kind: domain.BlobKindTranscript, Name: "manifest.txt"}
	if err := s.Blobs().Put(context.Background(), key, bytes.NewReader([]byte("hi"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Blobs().Open(context.Background(), key); err != nil {
		t.Fatalf("Open: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.dir, "blobs", systemBlobDir, key.Kind, key.Name))
	if err != nil {
		t.Fatalf("system blob file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob perm = %o, want 600", perm)
	}
}

func TestBlobOpenMissing(t *testing.T) {
	s := newStore(t)
	key := domain.BlobKey{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "nope.wav"}
	_, err := s.Blobs().Open(context.Background(), key)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestBlobDeleteIdempotent(t *testing.T) {
	s := newStore(t)
	key := domain.BlobKey{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "call.wav"}
	if err := s.Blobs().Delete(context.Background(), key); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := s.Blobs().Put(context.Background(), key, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Blobs().Delete(context.Background(), key); err != nil {
		t.Fatalf("delete existing: %v", err)
	}
	if _, err := s.Blobs().Open(context.Background(), key); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Open after delete: err = %v, want ErrNotFound", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	badIDs := []domain.OrderID{"../../etc/passwd", "..", "0123456789abcdef0123456789abcde", "ABCDEF0123456789ABCDEF0123456789"}
	for _, id := range badIDs {
		if _, err := s.Orders().Get(ctx, id); err == nil {
			t.Errorf("Get(%q) should fail", id)
		}
	}

	badBlobs := []domain.BlobKey{
		{OrderID: domain.NewOrderID(), Kind: "../../x", Name: "a"},
		{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "../x"},
		{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "/abs"},
		{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: ".."},
		{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: ""},
		{OrderID: "../../etc/passwd", Kind: domain.BlobKindRecording, Name: "a"},
		{OrderID: domain.NewOrderID(), Kind: domain.BlobKindRecording, Name: "a/b"},
	}
	for _, key := range badBlobs {
		if err := s.Blobs().Put(ctx, key, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Put(%+v) should fail", key)
		}
		if _, err := s.Blobs().Open(ctx, key); err == nil {
			t.Errorf("Open(%+v) should fail", key)
		}
		if err := s.Blobs().Delete(ctx, key); err == nil {
			t.Errorf("Delete(%+v) should fail", key)
		}
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, sampleOrder())
	entries, err := os.ReadDir(filepath.Join(s.dir, "orders"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestContextCancellation(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Orders().List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List err = %v, want context.Canceled", err)
	}
	o := mustCreate(t, s, sampleOrder())
	if _, err := s.Orders().Get(ctx, o.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v, want context.Canceled", err)
	}
}
