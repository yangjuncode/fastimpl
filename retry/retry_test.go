package retry

import (
	"errors"
	"testing"
)

func TestTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("glab api x: exit status 1: ERROR Get \"https://h/api\": dial tcp 1.2.3.4:443: i/o timeout"), true},
		{errors.New("gh api: TLS handshake timeout"), true},
		{errors.New("connect: connection refused"), true},
		{errors.New("read: connection reset by peer"), true},
		{errors.New("lookup h: no such host"), true},
		{errors.New("HTTP 503: Service Unavailable"), true},
		{errors.New("glab api: exit status 1: ERROR 404 Not Found"), false},
		{errors.New("issue 3 not found"), false},
		{errors.New("json: cannot decode"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := Transient(c.err); got != c.want {
			t.Errorf("Transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
