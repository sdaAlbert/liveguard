package browser

type SessionState struct {
	Configured bool `json:"configured"`
	Open       bool `json:"open"`
}
