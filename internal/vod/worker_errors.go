package vod

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// One adapter table owns worker -> state -> public error/HTTP mapping.
// HTTP applies to GET result; GET job/list always return a job view with 200.
// Reconcile means admission may have succeeded: do not release the lease until
// a snapshot or NOT_FOUND confirms the outcome. Never retain worker messages.
type workerFailure struct {
	State     string
	Code      string
	HTTP      int
	Reconcile bool
}

var workerFailures = map[string]workerFailure{
	"rpc:InvalidArgument":             {"failed", "bad_request", 400, false},
	"rpc:FailedPrecondition":          {"failed", "processing_unsupported", 422, false},
	"rpc:Unimplemented":               {"failed", "processing_unsupported", 422, false},
	"rpc:NotFound":                    {"paused", "", 409, false},
	"rpc:AlreadyExists":               {"processing", "", 409, true},
	"rpc:ResourceExhausted":           {"paused", "", 409, false},
	"rpc:Unavailable":                 {"processing", "", 409, true},
	"rpc:DeadlineExceeded":            {"processing", "", 409, true},
	"rpc:Canceled":                    {"processing", "", 409, true},
	"job:INPUT_NOT_FOUND":             {"failed", "storage_failed", 503, false},
	"job:INPUT_UNREADABLE":            {"failed", "invalid_media", 422, false},
	"job:INPUT_UNSUPPORTED":           {"failed", "unsupported_media_type", 415, false},
	"job:INPUT_TOO_LARGE":             {"failed", "upload_too_large", 413, false},
	"job:INPUT_TOO_LONG":              {"failed", "duration_exceeded", 422, false},
	"job:PATH_OUTSIDE_ROOT":           {"failed", "processing_failed", 500, false},
	"job:OUTPUT_EXISTS":               {"failed", "processing_failed", 500, false},
	"job:CHECKPOINT_INVALID":          {"failed", "processing_failed", 500, false},
	"job:CHECKPOINT_MISMATCH":         {"failed", "processing_failed", 500, false},
	"job:EMBEDDING_MODEL_MISMATCH":    {"failed", "processing_failed", 500, false},
	"job:WHITELIST_SESSION_NOT_FOUND": {"failed", "processing_failed", 500, false},
	"job:DECODER_FAILED":              {"failed", "invalid_media", 422, false},
	"job:ENCODER_FAILED":              {"failed", "processing_failed", 500, false},
	"job:MUX_FAILED":                  {"failed", "processing_failed", 500, false},
	"job:DISK_FULL":                   {"failed", "storage_failed", 503, false},
	"job:RUNTIME_FAILED":              {"paused", "", 409, false},
	"job:CANCELLED":                   {"cancelled", "job_cancelled", 409, false},
	"job:INTERNAL":                    {"failed", "processing_failed", 500, false},
}

func mapWorkerFailure(key string) workerFailure {
	if m, ok := workerFailures[key]; ok {
		return m
	}
	return workerFailures["job:INTERNAL"]
}
func mapRPCFailure(err error) workerFailure {
	key := "rpc:" + status.Code(err).String()
	if m, ok := workerFailures[key]; ok {
		return m
	}
	m := mapWorkerFailure("")
	// An unknown transport failure is not proof that Start was rejected.
	m.Reconcile = true
	return m
}
func workerMissing(err error) bool {
	return err != nil && status.Code(err) == codes.NotFound
}

// Stored public codes are allowlisted too, including rows from older servers.
func publicFailure(code string) workerFailure {
	for _, m := range workerFailures {
		if m.Code != "" && m.Code == code {
			return m
		}
	}
	return mapWorkerFailure("")
}
