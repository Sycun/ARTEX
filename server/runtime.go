package server

const (
	ModeAll    = "all"
	ModeAPI    = "api"
	ModeWorker = "worker"
)

// Runtime selects the process role. The zero value is the single-process server.
type Runtime struct {
	Mode        string
	WorkerID    string
	Zone        string
	Concurrency int
}
