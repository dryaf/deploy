package workerctl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"gopkg.in/yaml.v3"
)

type InventoryEdit struct {
	File     string
	Original []byte
	Document yaml.Node
	Mode     os.FileMode
}

func OpenInventoryEdit(file string) (*InventoryEdit, error) {
	absolute, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	dir, err := safefs.Open(filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.Open(filepath.Base(absolute), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	raw, err := safefs.ReadFile(dir, filepath.Base(absolute), 1<<20)
	if err != nil {
		return nil, err
	}
	inv, err := config.ParseInventory(absolute, raw)
	if err != nil {
		return nil, err
	}
	if inv.Version != 3 {
		return nil, fmt.Errorf("configure requires inventory version 3")
	}
	e := &InventoryEdit{File: absolute, Original: raw, Mode: info.Mode().Perm()}
	if err = yaml.Unmarshal(raw, &e.Document); err != nil {
		return nil, fmt.Errorf("invalid inventory YAML")
	}
	return e, nil
}

func (e *InventoryEdit) Set(server string, keys []string, value any) error {
	if !config.ValidInventoryName(server) || len(keys) == 0 {
		return fmt.Errorf("invalid inventory edit path")
	}
	all := append([]string{"servers", server}, keys...)
	if len(e.Document.Content) != 1 {
		return fmt.Errorf("invalid inventory document")
	}
	node := e.Document.Content[0]
	for i, key := range all {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("cannot edit non-mapping at %s", strings.Join(all[:i], "."))
		}
		var child *yaml.Node
		for j := 0; j < len(node.Content); j += 2 {
			if node.Content[j].Value == key {
				child = node.Content[j+1]
				break
			}
		}
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		if i == len(all)-1 {
			var next yaml.Node
			if err := next.Encode(value); err != nil {
				return err
			}
			next.HeadComment = child.HeadComment
			next.LineComment = child.LineComment
			next.FootComment = child.FootComment
			*child = next
			return nil
		}
		if key == "providers" && child.Kind == yaml.SequenceNode {
			converted := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: child.HeadComment, LineComment: child.LineComment, FootComment: child.FootComment}
			for _, item := range child.Content {
				converted.Content = append(converted.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: item.Value, HeadComment: item.HeadComment, LineComment: item.LineComment}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			}
			*child = converted
		}
		node = child
	}
	return nil
}

func (e *InventoryEdit) Candidate() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&e.Document); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if _, err := config.ParseInventory(e.File, buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (e *InventoryEdit) Save() error {
	candidate, err := e.Candidate()
	if err != nil {
		return err
	}
	if bytes.Equal(candidate, e.Original) {
		return nil
	}
	if e.Mode&0200 == 0 {
		return fmt.Errorf("inventory is read-only; save the candidate and rerun configure")
	}
	lock, err := safefs.AcquireLocalLock("worker-inventory", e.File)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir, err := safefs.Open(filepath.Dir(e.File))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = safefs.ReplaceFile(dir, filepath.Base(e.File), e.Original, candidate, uint32(e.Mode)); err != nil {
		return fmt.Errorf("inventory changed or could not be saved; original retained: %w", err)
	}
	e.Original = candidate
	return nil
}
