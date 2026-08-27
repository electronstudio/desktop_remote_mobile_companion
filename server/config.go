package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexflint/go-arg"
	"gopkg.in/yaml.v3"
)

const appName = "inara"

// configPath returns the default config file path, or empty if the user config
// directory cannot be determined.
func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, appName, "config.yaml")
}

// LoadConfig fills tag defaults, then overlays the default YAML config file.
// If the file does not exist it is created with all defaults commented out.
func LoadConfig(cli *CLI) error {
	path := configPath()
	if path == "" {
		return fillDefaults(cli)
	}
	return loadConfigFromPath(path, cli, true)
}

// loadConfigFromPath fills tag defaults and overlays the YAML config file at
// path. If createIfMissing is true and the file does not exist, it is created
// with all defaults commented out.
func loadConfigFromPath(path string, cli *CLI, createIfMissing bool) error {
	if err := fillDefaults(cli); err != nil {
		return err
	}

	if createIfMissing {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := writeDefaultConfig(path); err != nil {
				return fmt.Errorf("creating default config file: %w", err)
			}
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file %q: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cli); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return nil
}

// fillDefaults populates cli with the values from the struct-tag defaults,
// without reading environment variables or command-line arguments.
func fillDefaults(cli *CLI) error {
	p, err := arg.NewParser(arg.Config{IgnoreEnv: true}, cli)
	if err != nil {
		return err
	}
	return p.Parse(nil)
}

// writeDefaultConfig creates the config directory and writes the file with
// every default value present but commented out.
func writeDefaultConfig(path string) error {
	var defaults CLI
	if err := fillDefaults(&defaults); err != nil {
		return err
	}

	data, err := yaml.Marshal(defaults)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString("# " + appName + " configuration file\n"); err != nil {
		return err
	}
	if _, err := f.WriteString("# Uncomment lines to override defaults.\n\n"); err != nil {
		return err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			if _, err := f.WriteString("\n"); err != nil {
				return err
			}
			continue
		}
		if _, err := f.WriteString("# " + line + "\n"); err != nil {
			return err
		}
	}
	return nil
}
