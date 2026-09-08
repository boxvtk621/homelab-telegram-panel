package mobilegatewaybootstrap

import (
	"net/http"
	"regexp"
	"testing"
)

func TestDefaultRequestIDsMatchPrivateControllerProtocol(t *testing.T) {
	t.Parallel()
	ids := DefaultDependencies(http.NotFoundHandler(), 1001).IDs
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id, err := ids.New("request")
		if err != nil || !pattern.MatchString(id) || seen[id] {
			t.Fatalf("invalid or duplicate private request identity %q err=%v", id, err)
		}
		seen[id] = true
	}
}
