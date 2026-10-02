package domain

import "time"

type Event struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}
