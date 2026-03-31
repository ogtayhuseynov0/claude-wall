package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// secretStore manages key-value secrets stored on disk with restricted permissions.
// Values are never returned in full to the frontend — only masked versions.
type secretStore struct {
	mu      sync.RWMutex
	secrets map[string]string // name → value
}

var secrets = &secretStore{
	secrets: make(map[string]string),
}

func secretsFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.claude/claude-wall-secrets.json"
}

func (s *secretStore) load() {
	data, err := os.ReadFile(secretsFile())
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	json.Unmarshal(data, &s.secrets)
}

func (s *secretStore) save() {
	s.mu.RLock()
	data, _ := json.MarshalIndent(s.secrets, "", "  ")
	s.mu.RUnlock()
	os.WriteFile(secretsFile(), data, 0600)
}

// resolve dereferences a value: if it starts with $, look up the secret by name.
// Otherwise return the value as-is (raw input from user).
func (s *secretStore) resolve(val string) string {
	if !strings.HasPrefix(val, "$") {
		return val
	}
	name := val[1:]
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.secrets[name]; ok {
		return v
	}
	return val // unresolved, return as-is
}

// mask returns a masked version of a value for display.
func mask(val string) string {
	if len(val) <= 4 {
		return strings.Repeat("*", len(val))
	}
	return val[:2] + strings.Repeat("*", len(val)-4) + val[len(val)-2:]
}

// listMasked returns all secrets with masked values.
func (s *secretStore) listMasked() []map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]map[string]string, 0, len(s.secrets))
	for name, val := range s.secrets {
		result = append(result, map[string]string{
			"name":  name,
			"value": mask(val),
		})
	}
	return result
}

// names returns just the secret names (for dropdowns), sorted for stable output.
func (s *secretStore) names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, 0, len(s.secrets))
	for name := range s.secrets {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func handleSecretsAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"secrets": secrets.listMasked(),
			"names":   secrets.names(),
		})

	case "POST":
		var req struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || req.Value == "" {
			http.Error(w, "name and value required", 400)
			return
		}
		secrets.mu.Lock()
		secrets.secrets[req.Name] = req.Value
		secrets.mu.Unlock()
		secrets.save()
		w.Write([]byte(`{"status":"ok"}`))

	case "DELETE":
		name := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
		if name == "" {
			http.Error(w, "missing name", 400)
			return
		}
		secrets.mu.Lock()
		delete(secrets.secrets, name)
		secrets.mu.Unlock()
		secrets.save()
		w.Write([]byte(`{"status":"ok"}`))

	default:
		http.Error(w, "method not allowed", 405)
	}
}
