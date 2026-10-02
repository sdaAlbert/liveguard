package agent

import (
	"net/http"
	"time"
)

func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 25 * time.Second}
}
