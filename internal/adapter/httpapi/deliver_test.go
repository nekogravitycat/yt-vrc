package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
	"github.com/nekogravitycat/yt-vrc/internal/domain/port"
	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
	"github.com/nekogravitycat/yt-vrc/internal/usecase/playvideo"
)

// --- fakes ----------------------------------------------------------

// recordingMessages stands in for the renderer, keeping what it was
// asked to draw instead of encoding it.
type recordingMessages struct {
	mu   sync.Mutex
	deck message.Deck
	spec video.OutputSpec
}

func (m *recordingMessages) Render(_ context.Context, deck message.Deck, spec video.OutputSpec) (*video.MediaAsset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deck, m.spec = deck, spec
	return &video.MediaAsset{Key: video.CacheKey("rendered_" + string(spec.Container)), Spec: spec}, nil
}

func (m *recordingMessages) Open(video.CacheKey, string) (io.ReadSeekCloser, time.Time, error) {
	return nil, time.Time{}, os.ErrNotExist
}

func (m *recordingMessages) last() (message.Deck, video.OutputSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deck, m.spec
}

// blockingResolver never answers, which is what a long video looks like
// for the first several seconds.
type blockingResolver struct{ entered chan struct{} }

func (b *blockingResolver) Resolve(ctx context.Context, _ video.ID, _ video.OutputSpec) (*video.Resolution, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// stubPackager exists only to get past prepare's "is this container
// deliverable" check, which runs before any resolve.
type stubPackager struct{}

func (stubPackager) Container() video.Container { return video.ContainerHLS }

func (stubPackager) Package(_ context.Context, res *video.Resolution, _, _, dir string) (*video.MediaAsset, error) {
	return &video.MediaAsset{VideoID: res.VideoID, Dir: dir, State: video.StateReady}, nil
}

type emptyStore struct{}

func (emptyStore) Get(video.CacheKey) (*video.MediaAsset, bool) { return nil, false }
func (emptyStore) Put(*video.MediaAsset) error                  { return nil }
func (emptyStore) Drop(video.CacheKey) error                    { return nil }
func (emptyStore) Purge() error                                 { return nil }
func (emptyStore) List(int) []*video.MediaAsset                 { return nil }
func (emptyStore) Dir(video.CacheKey) (string, error)           { return os.TempDir(), nil }
func (emptyStore) Open(video.CacheKey, string) (io.ReadSeekCloser, time.Time, error) {
	return nil, time.Time{}, os.ErrNotExist
}

func testServer(t *testing.T, msgs *recordingMessages, res *blockingResolver) *Server {
	t.Helper()
	return &Server{
		Play: &playvideo.UseCase{
			Resolver:  res,
			Store:     emptyStore{},
			Packagers: map[video.Container]port.Packager{video.ContainerHLS: stubPackager{}},
			Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			TempDir:   t.TempDir(),
			MaxJobs:   2,
		},
		Messages: msgs,
		Defaults: Defaults{
			Container:        video.ContainerHLS,
			Quality:          1080,
			MaxQuality:       1080,
			MessageContainer: video.ContainerMP4,
		},
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		StateDir:     t.TempDir(),
		PrepareGrace: 150 * time.Millisecond,
	}
}

// --- tests ----------------------------------------------------------

// A message is a still frame, so it defaults to the container that
// delivers in one request -- but an extension on the URL still wins.
func TestMessageContainerDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		path string
		want video.Container
	}{
		{"/h", video.ContainerMP4},
		{"/h.m3u8", video.ContainerHLS},
		{"/h.mp4", video.ContainerMP4},
		{"/status", video.ContainerMP4},
		{"/status.m3u8", video.ContainerHLS},
	} {
		msgs := &recordingMessages{}
		s := testServer(t, msgs, &blockingResolver{})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if _, spec := msgs.last(); spec.Container != tc.want {
			t.Errorf("%s rendered as %s, want %s", tc.path, spec.Container, tc.want)
		}
	}
}

// The headline behaviour: a video that isn't ready within the grace
// period answers with a progress frame instead of holding the connection
// open until the player calls the URL broken.
func TestSlowPrepareAnswersWithProgressFrame(t *testing.T) {
	msgs := &recordingMessages{}
	res := &blockingResolver{entered: make(chan struct{}, 1)}
	s := testServer(t, msgs, res)

	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dQw4w9WgXcQ", nil))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request never returned; the grace period did not bound the wait")
	}

	deck, spec := msgs.last()
	if len(deck) != 1 {
		t.Fatalf("want one progress frame, got %d", len(deck))
	}
	if deck[0].Kind != message.KindProgress {
		t.Errorf("frame kind = %q, want %q", deck[0].Kind, message.KindProgress)
	}
	if !strings.Contains(strings.ToLower(deck[0].Title), "preparing") {
		t.Errorf("frame title = %q, want it to say the video is being prepared", deck[0].Title)
	}
	if spec.Container != video.ContainerMP4 {
		t.Errorf("progress frame delivered as %s, want the message container", spec.Container)
	}

	// The job must outlive the request that gave up on it -- that is what
	// makes "try the same URL again" work.
	select {
	case <-res.entered:
	default:
		t.Error("resolve never started")
	}
}

// A grace period that swallowed a real failure would report "come back
// later" for a video that is never going to play.
func TestFastFailureStillReportsTheError(t *testing.T) {
	msgs := &recordingMessages{}
	s := testServer(t, msgs, &blockingResolver{})
	s.Play.Resolver = failingResolver{}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dQw4w9WgXcQ", nil))

	deck, _ := msgs.last()
	if len(deck) == 0 {
		t.Fatal("nothing rendered")
	}
	if deck[0].Kind == message.KindProgress {
		t.Errorf("a failed resolve rendered as progress: %+v", deck[0])
	}
}

type failingResolver struct{}

func (failingResolver) Resolve(context.Context, video.ID, video.OutputSpec) (*video.Resolution, error) {
	return nil, video.ErrNotFound
}
