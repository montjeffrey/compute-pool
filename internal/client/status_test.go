package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestStatusAnswersAreBounded(t *testing.T) {
	huge := `{"node":"` + strings.Repeat("a", maxStatusBytes+1024) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(huge))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := New()
	c.Port, _ = strconv.Atoi(port)
	if _, err := c.Info(context.Background(), host); err == nil || !strings.Contains(err.Error(), "decode info") {
		t.Errorf("Info accepted an oversized answer: %v", err)
	}
	if _, err := c.Jobs(context.Background(), host); err == nil || !strings.Contains(err.Error(), "decode jobs") {
		t.Errorf("Jobs accepted an oversized answer: %v", err)
	}
}
