package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Keep a stable lock inode beside the config: locking config.toml itself would
// stop serializing writers as soon as the first atomic rename replaced it.
func lockConfigFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockConfigHandle(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	unlock := func() { _ = f.Close() }
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, err
	}
	if err == nil && !info.Mode().IsRegular() {
		unlock()
		return nil, fmt.Errorf("config must be a regular file, not a symlink or directory")
	}
	return unlock, nil
}

func updateConfigFile(path string, change func(map[string]any) error) error {
	unlock, err := lockConfigFile(path)
	if err != nil {
		return errInitWrite(path, err)
	}
	defer unlock()
	doc, err := configSettingsDocument(path)
	if err != nil {
		return err
	}
	if err := checkConfigCase(doc, reflect.TypeFor[fileConfig]()); err != nil {
		return err
	}
	if err := change(doc); err != nil {
		return err
	}
	data, err := toml.Marshal(doc)
	if err != nil {
		return errConfigInvalid(path, err)
	}
	var file fileConfig
	if err := toml.Unmarshal(data, &file); err != nil {
		return errConfigInvalid(path, err)
	}
	if err := file.Sessions.validate(); err != nil {
		return errConfigInvalid(path, err)
	}
	return placeConfigFile(path, data, true)
}

// Only known fields have case-insensitive decoding. Unknown keys retain their
// TOML case-sensitive meaning, including inside unknown tables.
func checkConfigCase(doc map[string]any, typ reflect.Type) error {
	for i := range typ.NumField() {
		field := typ.Field(i)
		key := strings.Split(field.Tag.Get("toml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		name, err := configSettingsSpelling(doc, key)
		if err != nil {
			return err
		}
		if table, ok := doc[name].(map[string]any); ok && field.Type.Kind() == reflect.Struct {
			if err := checkConfigCase(table, field.Type); err != nil {
				return err
			}
		}
	}
	return nil
}

func updateConfigDestination(path, server, key string, force bool) error {
	return updateConfigFile(path, func(doc map[string]any) error {
		urlName, err := configSettingsSpelling(doc, "url")
		if err != nil {
			return err
		}
		keyName, err := configSettingsSpelling(doc, "api_key")
		if err != nil {
			return err
		}
		if !force {
			for _, name := range []string{urlName, keyName} {
				if value, exists := doc[name]; exists && value != "" {
					return errInitExists(path)
				}
			}
		}
		doc[urlName] = server
		if key != "" || force {
			doc[keyName] = key
		}
		return nil
	})
}
