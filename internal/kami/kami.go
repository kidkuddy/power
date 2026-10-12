// Package kami holds the parts of Kami's HTTP API that POWER reads. POWER is a client of the
// daemon's socket API, so it keeps its own copy of the wire types instead of importing Kami.
package kami

import (
	"encoding/json"
	"time"
)

// StDone is a finished run's status.
const StDone = "done"

// Envelope wraps every API reply.
type Envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *APIError       `json:"error,omitempty"`
}

// APIError is the error half of an Envelope.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Contract is a decision a run waits on.
type Contract struct {
	ID         string            `json:"id"`
	RunID      string            `json:"run_id"`
	Step       string            `json:"step"`
	Kekkai     string            `json:"kekkai"`
	Origin     string            `json:"origin"` // state | worker | engine
	Kind       string            `json:"kind"`
	Title      string            `json:"title"`
	Body       string            `json:"body,omitempty"`
	Options    []string          `json:"options,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
	Default    string            `json:"default,omitempty"`
	Lock       bool              `json:"lock"`
	Status     string            `json:"status"` // open | resolved | cancelled
	Deadline   *time.Time        `json:"deadline,omitempty"`
	Resolution *Resolution       `json:"resolution,omitempty"`
}

// Resolution is how a contract was answered.
type Resolution struct {
	Answer string         `json:"answer"`
	Data   map[string]any `json:"data,omitempty"`
	By     string         `json:"by"` // human | resolver
	Note   string         `json:"note,omitempty"`
}

// ContractView is a contract with its effective AFK mode and workflow.
type ContractView struct {
	*Contract
	Effective string `json:"effective_mode"`
	Workflow  string `json:"workflow,omitempty"`
}

// Event is one entry of the daemon's event stream.
type Event struct {
	ID     int64          `json:"id"`
	TS     time.Time      `json:"ts"`
	Kekkai string         `json:"kekkai,omitempty"`
	Run    string         `json:"run,omitempty"`
	Type   string         `json:"type"`
	Data   map[string]any `json:"data,omitempty"`
}
