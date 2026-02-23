// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// gitea-robot CLI - thin wrapper for Gitea Robot API
// Usage: go run cmd/gitea-robot/main.go [command] [flags]

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

var (
	giteaURL   = os.Getenv("GITEA_URL")
	giteaToken = os.Getenv("GITEA_TOKEN")
)

func main() {
	if giteaURL == "" {
		giteaURL = "http://localhost:3000"
	}
	if giteaToken == "" {
		fmt.Fprintln(os.Stderr, "Error: GITEA_TOKEN environment variable required")
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	os.Args = os.Args[1:] // Remove command from args

	switch command {
	case "triage":
		triageCmd()
	case "ready":
		readyCmd()
	case "graph":
		graphCmd()
	case "add-dep":
		addDepCmd()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`gitea-robot - CLI for Gitea Robot API

Usage:
  gitea-robot [command] [flags]

Commands:
  triage      Get prioritized task list
  ready       Get unblocked (ready) tasks
  graph       Get dependency graph
  add-dep     Add dependency between issues

Environment:
  GITEA_URL    Gitea instance URL (default: http://localhost:3000)
  GITEA_TOKEN  API token for authentication

Examples:
  # Get triage report
  gitea-robot triage --owner terraphim --repo gitea

  # Get ready issues
  gitea-robot ready --owner terraphim --repo gitea

  # Add dependency: issue 2 blocked by issue 1
  gitea-robot add-dep --owner terraphim --repo gitea --issue 2 --blocks 1`)
}

func triageCmd() {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	format := fs.String("format", "json", "Output format: json or markdown")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/triage?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)

	if *format == "json" {
		fmt.Println(data)
	} else {
		// Pretty print as markdown
		var result map[string]interface{}
		json.Unmarshal([]byte(data), &result)
		printTriageMarkdown(result)
	}
}

func readyCmd() {
	fs := flag.NewFlagSet("ready", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/ready?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)
	fmt.Println(data)
}

func graphCmd() {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/graph?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)
	fmt.Println(data)
}

func addDepCmd() {
	fs := flag.NewFlagSet("add-dep", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	issue := fs.Int64("issue", 0, "Issue ID (the one being blocked)")
	blocks := fs.Int64("blocks", 0, "Issue ID that blocks this issue")
	relatesTo := fs.Int64("relates-to", 0, "Issue ID that relates to this issue")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" || *issue == 0 {
		fmt.Fprintln(os.Stderr, "Error: --owner, --repo, and --issue required")
		fs.Usage()
		os.Exit(1)
	}

	depType := "blocks"
	dependsOn := *blocks
	if *relatesTo > 0 {
		depType = "relates_to"
		dependsOn = *relatesTo
	}
	if dependsOn == 0 {
		fmt.Fprintln(os.Stderr, "Error: --blocks or --relates-to required")
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/dependencies", giteaURL, *owner, *repo, *issue)
	body := fmt.Sprintf(`{"depends_on": %d, "dep_type": "%s"}`, dependsOn, depType)
	
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "token "+giteaToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		fmt.Println("✓ Dependency added successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n%s\n", resp.Status, string(body))
		os.Exit(1)
	}
}

func apiGet(url string) string {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("Authorization", "token "+giteaToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error making request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Error: %s\n%s\n", resp.Status, string(body))
		os.Exit(1)
	}

	return string(body)
}

func printTriageMarkdown(result map[string]interface{}) {
	fmt.Println("## Triage Report")
	fmt.Println()

	if quickRef, ok := result["quick_ref"].(map[string]interface{}); ok {
		fmt.Printf("**Stats:** Total: %.0f, Open: %.0f, Blocked: %.0f, Ready: %.0f\n\n",
			quickRef["total"], quickRef["open"], quickRef["blocked"], quickRef["ready"])
	}

	if recs, ok := result["recommendations"].([]interface{}); ok {
		fmt.Println("### Top Recommendations")
		for i, r := range recs {
			if i >= 5 {
				break
			}
			rec := r.(map[string]interface{})
			fmt.Printf("%d. **#%.0f: %s** (PageRank: %.4f)\n",
				i+1, rec["index"], rec["title"], rec["pagerank"])
		}
	}
}