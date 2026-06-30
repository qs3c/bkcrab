// Command gen fetches models.dev and writes a slim runtime catalog.
//
// Usage:
//
//	go run ./internal/modelspec/gen -out internal/modelspec/catalog.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const source = "https://models.dev/api.json"

type mdevModel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

type mdevProvider struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	API    string               `json:"api"`
	Models map[string]mdevModel `json:"models"`
}

type entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	APIHost string `json:"apiHost"`
}

type catalogFile struct {
	Entries []entry `json:"entries"`
}

func apiHost(api string) string {
	api = strings.TrimSpace(api)
	if api == "" {
		return ""
	}
	if !strings.Contains(api, "://") {
		api = "https://" + api
	}
	u, err := url.Parse(api)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func main() {
	out := flag.String("out", "catalog.json", "output path for the slim catalog")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch %s: %v\n", source, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "fetch %s: status %d\n", source, resp.StatusCode)
		os.Exit(1)
	}

	var providers map[string]mdevProvider
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}

	var entries []entry
	for _, p := range providers {
		host := apiHost(p.API)
		for _, m := range p.Models {
			if m.Limit.Context <= 0 && m.Limit.Output <= 0 {
				continue
			}
			entries = append(entries, entry{
				ID:      m.ID,
				Name:    m.Name,
				Context: m.Limit.Context,
				Output:  m.Limit.Output,
				APIHost: host,
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		if entries[i].APIHost != entries[j].APIHost {
			return entries[i].APIHost < entries[j].APIHost
		}
		return entries[i].Name < entries[j].Name
	})

	buf, err := json.MarshalIndent(catalogFile{Entries: entries}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d entries to %s\n", len(entries), *out)
}
