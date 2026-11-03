package safefs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func WritePrivateJSON(destination string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(destination), ".deploy-json-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), destination); err != nil {
		return err
	}
	return SyncFile(filepath.Dir(destination))
}

func DecodeJSON(data []byte, dest any, limit int) error {
	if len(data) > limit {
		return fmt.Errorf("JSON exceeds protocol limit")
	}
	if err := RejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return fmt.Errorf("invalid protocol JSON: %w", err)
	}
	return nil
}

func ReadJSON(r io.Reader, dest any, limit int) error {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return err
	}
	return DecodeJSON(data, dest, limit)
}

func RejectDuplicateJSONKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return fmt.Errorf("JSON nesting exceeds limit")
		}
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON")
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return fmt.Errorf("invalid JSON object")
				}
				k, ok := key.(string)
				if !ok || keys[k] {
					return fmt.Errorf("duplicate or invalid JSON key")
				}
				keys[k] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
