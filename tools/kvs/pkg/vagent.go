package pkg

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agentEndpoint describes the usable endpoint fields parsed from one
// vsc_agents_dir JSON file (json.endpoint.path / json.endpoint.port /
// json.connectionToken).
type agentEndpoint struct {
	Path  string
	Port  int
	Token string
}

// parseAgentEndpoint reads a single vsc_agents_dir JSON file and extracts the
// endpoint fields. ok is false when the file is missing/unparseable or has no
// usable endpoint (neither path nor port).
func parseAgentEndpoint(file string) (ep agentEndpoint, ok bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		log.Printf("[agents] read %s: %v", file, err)
		return ep, false
	}
	var parsed struct {
		Endpoint struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"endpoint"`
		ConnectionToken string `json:"connectionToken"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Printf("[agents] parse %s: %v", file, err)
		return ep, false
	}
	ep = agentEndpoint{
		Path:  parsed.Endpoint.Path,
		Port:  parsed.Endpoint.Port,
		Token: parsed.ConnectionToken,
	}
	return ep, ep.Path != "" || ep.Port > 0
}

// AgentBridgeArgsFromFile builds the bridge suffix arguments for the agent host
// command by re-reading the endpoint JSON file named by the ?agent= parameter:
//   - socket: --agent-host-bridge-path {path} --agent-host-bridge-connection-token {token}
//   - tcp:    --agent-host-bridge-port {port} --agent-host-bridge-connection-token {token}
//
// Returns "" when the file cannot be read/parsed or has no usable endpoint.
func AgentBridgeArgsFromFile(file string) string {
	ep, ok := parseAgentEndpoint(file)
	if !ok {
		return ""
	}
	var b strings.Builder
	if ep.Path != "" {
		b.WriteString(" --agent-host-bridge-path ")
		b.WriteString(ep.Path)
	} else if ep.Port > 0 {
		b.WriteString(" --agent-host-bridge-port ")
		b.WriteString(strconv.Itoa(ep.Port))
		b.WriteString("--agent-host-bridge-host 127.0.0.1")
	}
	if ep.Token != "" {
		b.WriteString(" --agent-host-bridge-connection-token ")
		b.WriteString(ep.Token)
	}
	return b.String()
}

// ListAgentEntries scans the vsc_agents_dir directory and returns {file, name}
// pairs for every *.json file with a usable endpoint (path or port). name is
// the socket path, or 127.0.0.1:{port} for tcp endpoints. Unreadable/unparseable
// files are skipped with a warning; nil if the directory is unset or unreadable.
func ListAgentEntries(dir string) []map[string]string {
	if dir == "" {
		return nil
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[agents] read dir %s: %v", dir, err)
		return nil
	}
	var out []map[string]string
	type entry struct {
		file, name string
		mod        time.Time
	}
	var list []entry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(strings.ToLower(f.Name()), ".json") {
			continue
		}
		ep, ok := parseAgentEndpoint(filepath.Join(dir, f.Name()))
		if !ok {
			continue
		}
		name := ep.Path
		if name == "" && ep.Port > 0 {
			name = "127.0.0.1:" + strconv.Itoa(ep.Port)
		}
		info, err := f.Info()
		var mod time.Time
		if err == nil && info != nil {
			mod = info.ModTime()
		}
		list = append(list, entry{file: f.Name()[:len(f.Name())-5], name: name, mod: mod})
	}
	// Newest first.
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	for _, it := range list {
		out = append(out, map[string]string{
			"file": it.file,
			"name": it.name,
			"time": it.mod.Format("01-02 15:04"),
		})
	}
	return out
}
