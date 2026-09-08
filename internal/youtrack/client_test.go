package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testToken = "perm:synthetic-test-token-not-a-credential"

func fixture(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "0-1", "HL")
	if err != nil {
		t.Fatal(err)
	}
	c.transport.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(c.Close)
	return c
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func issue() Issue {
	return Issue{ID: "2-1", ReadableID: "HL-210", Summary: "Test", Project: Project{ID: "0-1", Key: "HL"}, Updated: 1000, Fields: []Field{}}
}

func TestOnlyYouTrackRoutesAndProject(t *testing.T) {
	var paths []string
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("credential missing")
		}
		if r.URL.Query().Get("fields") == "" {
			t.Error("missing projection")
		}
		switch r.URL.Path {
		case "/api/users/me":
			jsonReply(w, User{ID: "1-1", Login: "owner"})
		case "/api/issues":
			if r.URL.Query().Get("query") != "project: {HL} sort by: updated desc" || r.URL.Query().Get("$skip") != "30" || r.URL.Query().Get("$top") != "30" {
				t.Error("unbounded/wrong query")
			}
			jsonReply(w, []Issue{issue()})
		case "/api/issues/HL-210":
			jsonReply(w, issue())
		case "/api/issues/2-1/comments":
			if r.Method == "POST" {
				jsonReply(w, Comment{ID: "7-1", Text: "Hello", Created: 1000})
			} else {
				jsonReply(w, []Comment{})
			}
		case "/api/admin/projects/0-1/articles":
			jsonReply(w, []Article{{ID: "226-1", ReadableID: "HL-A-23", Project: issue().Project}})
		case "/api/articles/HL-A-23":
			jsonReply(w, Article{ID: "226-1", ReadableID: "HL-A-23", Project: issue().Project})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	if _, err := c.Me(ctx, testToken); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Issues(ctx, testToken, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Comments(ctx, testToken, "HL-210", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddComment(ctx, testToken, "HL-210", "Hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Articles(ctx, testToken, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Article(ctx, testToken, "HL-A-23"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 8 {
		t.Fatalf("calls=%v", paths)
	}
}

func TestRejectsForeignAndMalformedObjects(t *testing.T) {
	var requests atomic.Int32
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		v := issue()
		v.Project.ID = "0-2"
		jsonReply(w, v)
	})
	for _, id := range []string{"OTHER-1", "HL-01", "HL-1/../users/me", "HL-1?x=y", "HL-A-1"} {
		if _, err := c.Issue(context.Background(), testToken, id); !errors.Is(err, ErrDenied) {
			t.Error(id, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid target caused network")
	}
	if _, err := c.Issue(context.Background(), testToken, "HL-210"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if _, err := c.AddComment(context.Background(), testToken, "HL-210", "text"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("foreign target caused POST")
	}
}

func TestRedirectsAndAmbiguousWritesAreNotRetried(t *testing.T) {
	var leaked, posts atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues/HL-210" {
			jsonReply(w, issue())
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Location", other.URL)
		w.WriteHeader(307)
	})
	if _, err := c.Me(context.Background(), testToken); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if leaked.Load() != 0 {
		t.Fatal("token redirected")
	}
	if _, err := c.AddComment(context.Background(), testToken, "HL-210", "Hello"); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatal("POST was retried")
	}
}

func TestUntrustedResponsesAndBoundedErrors(t *testing.T) {
	for _, body := range []string{`{"id":"1-1","login":"owner","login":"other"}`, `{"id":"1-1","login":"\ud800"}`, strings.Repeat("x", maxResponse+1), `null`, `not-json`} {
		t.Run(body[:min(len(body), 25)], func(t *testing.T) {
			c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			_, err := c.Me(context.Background(), testToken)
			if err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatal("untrusted response accepted/leaked")
			}
		})
	}
	for _, endpoint := range []string{"http://youtrack.test", "https://user:pass@youtrack.test", "https://youtrack.test/path", "https://youtrack.test?q=x"} {
		if c, err := New(endpoint, "0-1", "HL"); err == nil {
			c.Close()
			t.Error("unsafe endpoint accepted")
		}
	}
}
