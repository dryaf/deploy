package install

import "encoding/json"

// RuntimeInput is an unresolved, serializable installation document. Route
// revisions are labels until Resolve binds them to provider destinations. New
// coordinators retain these bytes, never re-serialize a resolved Runtime as input.
// The distinct type prevents accidental double resolution in typed adapters.
type RuntimeInput Runtime

func (input RuntimeInput) Bytes() ([]byte, error) { return json.Marshal(input) }
func (input RuntimeInput) Resolve() (Runtime, error) {
	raw, err := input.Bytes()
	if err != nil {
		return Runtime{}, err
	}
	return decodeRuntime(raw)
}
