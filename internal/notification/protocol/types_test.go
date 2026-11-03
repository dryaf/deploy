package protocol

import "testing"

func TestStrictDecoding(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"v":1,"v":1}`), []byte(`{"v":1,"unknown":true}`), []byte(`{"v":1} {}`), {'{', '"', 'o', 'p', '"', ':', '"', 0xff, '"', '}'}} {
		var r Request
		if Decode(body, &r, MaxRequest) == nil {
			t.Errorf("accepted %q", body)
		}
	}
}
