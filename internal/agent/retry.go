package agent

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// DoModelRequest retries a replayable provider request at most once. It never
// retries business tools or commits; a provider may still bill both attempts.
func DoModelRequest(client *http.Client, req *http.Request) (*http.Response, int, error) {
	return doModelRequest(client, req, 250*time.Millisecond)
}

func doModelRequest(client *http.Client, req *http.Request, delay time.Duration) (*http.Response, int, error) {
	for attempt := 0; ; attempt++ {
		if err := req.Context().Err(); err != nil {
			return nil, attempt, err
		}
		response, err := client.Do(req)
		retry := false
		if err != nil {
			var transport net.Error
			retry = errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || (errors.As(err, &transport) && (transport.Timeout() || transport.Temporary()))
		} else {
			switch response.StatusCode {
			case 408, 429, 500, 502, 503, 504:
				retry = true
			}
		}
		if attempt >= 1 || !retry || req.Context().Err() != nil || (req.Body != nil && req.GetBody == nil) {
			return response, attempt, err
		}
		wait := delay
		if response != nil && response.Header.Get("Retry-After") != "" {
			value := response.Header.Get("Retry-After")
			if seconds, parseErr := strconv.Atoi(value); parseErr == nil {
				if seconds < 0 || seconds > 2 {
					return response, attempt, err
				}
				wait = max(wait, time.Duration(seconds)*time.Second)
			} else if at, parseErr := http.ParseTime(value); parseErr == nil {
				remaining := time.Until(at)
				if remaining > 2*time.Second {
					return response, attempt, err
				}
				wait = max(wait, remaining)
			} else {
				return response, attempt, err
			}
		}
		if deadline, ok := req.Context().Deadline(); ok && time.Until(deadline) <= wait {
			return response, attempt, err
		}
		if response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
		}
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, attempt, req.Context().Err()
		case <-timer.C:
		}
		next := req.Clone(req.Context())
		if req.GetBody != nil {
			next.Body, err = req.GetBody()
			if err != nil {
				return nil, attempt, err
			}
		}
		req = next
	}
}
