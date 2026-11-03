package interaction

// Rejection is a permanent input/policy decision safe to disclose to the
// authenticated conversation. Storage and transport errors must not use it.
type Rejection struct {
	Message string
	Silent  bool
}

func (r *Rejection) Error() string       { return r.Message }
func Reject(message string) error        { return &Rejection{Message: message} }
func RejectQuietly(message string) error { return &Rejection{Message: message, Silent: true} }
