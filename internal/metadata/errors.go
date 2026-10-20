package metadata

type ErrorCode string

const (
	InvalidID          ErrorCode = "invalid_id"
	ValidationFailed   ErrorCode = "validation_failed"
	NotFound           ErrorCode = "not_found"
	DuplicateEmail     ErrorCode = "duplicate_email"
	InvalidReference   ErrorCode = "invalid_reference"
	MaintainerInUse    ErrorCode = "maintainer_in_use"
	PreconditionFailed ErrorCode = "precondition_failed"
	Unavailable        ErrorCode = "unavailable"
	InternalError      ErrorCode = "internal_error"
)

type Error struct {
	Code    ErrorCode
	Message string
	Fields  map[string]string
	cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.cause }

func NewError(code ErrorCode, fields map[string]string) *Error {
	messages := map[ErrorCode]string{
		InvalidID:          "A canonical lowercase UUIDv4 is required.",
		ValidationFailed:   "One or more fields are invalid.",
		NotFound:           "The resource was not found.",
		DuplicateEmail:     "A maintainer with this email already exists.",
		InvalidReference:   "The maintainer does not exist.",
		MaintainerInUse:    "The maintainer is referenced by an application.",
		PreconditionFailed: "No supplied revision matches the resource.",
		Unavailable:        "Storage is temporarily unavailable.",
		InternalError:      "An internal error occurred.",
	}
	return &Error{Code: code, Message: messages[code], Fields: fields}
}

func WrapError(code ErrorCode, cause error) *Error {
	err := NewError(code, nil)
	err.cause = cause
	return err
}
