package downloader

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const interactiveHelp = `
Commands:
  sa <query>   Search albums
  st <query>   Search tracks
  sr <query>   Search artists
  sp <query>   Search playlists
  dl <url>     Add a Qobuz or Last.fm URL directly to the queue
  q            Show the current queue
  rm <n>       Remove item n from the queue
  clear        Clear the entire queue
  go           Start downloading everything in the queue
  help         Show this help
  exit         Quit (also Ctrl+C or Ctrl+D)
`

// Interactive runs a REPL that lets the user search, build a queue, and
// download — all without leaving the session. No external dependencies.
func (d *Downloader) Interactive() {
	reader := bufio.NewReader(os.Stdin)
	var queue []SearchResult

	fmt.Println("\033[33mqobuz-dl interactive mode — type 'help' for commands\033[0m")

	for {
		fmt.Print("\033[36mqobuz\033[0m > ")
		line, err := reader.ReadString('\n')
		if err != nil { // EOF / Ctrl+D
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		cmd, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)

		switch strings.ToLower(cmd) {

		case "sa":
			queue = interactiveSearch(d, "album", arg, queue)
		case "st":
			queue = interactiveSearch(d, "track", arg, queue)
		case "sr":
			queue = interactiveSearch(d, "artist", arg, queue)
		case "sp":
			queue = interactiveSearch(d, "playlist", arg, queue)

		case "dl":
			if arg == "" {
				fmt.Println("\033[31mUsage: dl <url>\033[0m")
				continue
			}
			queue = append(queue, SearchResult{Text: arg, URL: arg})
			fmt.Printf("\033[32m  ✓ Added to queue (total: %d)\033[0m\n", len(queue))

		case "q", "queue":
			printQueue(queue)

		case "rm":
			queue = interactiveRemove(queue, arg)

		case "clear":
			queue = nil
			fmt.Println("\033[33m  Queue cleared.\033[0m")

		case "go", "download":
			if len(queue) == 0 {
				fmt.Println("\033[33m  Queue is empty.\033[0m")
				continue
			}
			urls := make([]string, len(queue))
			for i, r := range queue {
				urls[i] = r.URL
			}
			queue = nil
			d.DownloadURLs(urls)

		case "help":
			fmt.Print(interactiveHelp)

		case "exit", "quit", "q!":
			fmt.Println("\033[33mBye!\033[0m")
			return

		default:
			fmt.Printf("\033[31m  Unknown command %q — type 'help' for a list\033[0m\n", cmd)
		}
	}
}

// interactiveSearch runs a search, prints numbered results, and lets the user
// pick items to add to the queue. Returns the updated queue.
func interactiveSearch(d *Downloader, itemType, query string, queue []SearchResult) []SearchResult {
	if query == "" {
		fmt.Printf("\033[31m  Usage: s%s <query>\033[0m\n", string(itemType[0]))
		return queue
	}

	results, err := Search(d.Client, itemType, query, 20)
	if err != nil {
		fmt.Printf("\033[31m  Search error: %v\033[0m\n", err)
		return queue
	}
	if len(results) == 0 {
		fmt.Println("\033[90m  No results found.\033[0m")
		return queue
	}

	fmt.Println()
	for i, r := range results {
		fmt.Printf("  \033[90m[%2d]\033[0m %s\n", i+1, r.Text)
	}
	fmt.Print("\nPick numbers to queue (e.g. 1 3 5), or Enter to skip: ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return queue
	}

	added := 0
	for _, tok := range strings.Fields(line) {
		n, err := strconv.Atoi(tok)
		if err == nil && n >= 1 && n <= len(results) {
			queue = append(queue, results[n-1])
			added++
		}
	}
	if added > 0 {
		fmt.Printf("\033[32m  ✓ %d item(s) added to queue (total: %d)\033[0m\n\n", added, len(queue))
	}
	return queue
}

// printQueue prints the current queue with index numbers.
func printQueue(queue []SearchResult) {
	if len(queue) == 0 {
		fmt.Println("\033[90m  Queue is empty.\033[0m")
		return
	}
	fmt.Printf("\n\033[33mQueue (%d item(s)):\033[0m\n", len(queue))
	for i, r := range queue {
		label := r.Text
		if label == r.URL {
			label = r.URL // direct URL entry
		}
		fmt.Printf("  \033[90m[%d]\033[0m %s\n", i+1, label)
	}
	fmt.Println()
}

// interactiveRemove removes item n (1-based) from the queue.
func interactiveRemove(queue []SearchResult, arg string) []SearchResult {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 || n > len(queue) {
		fmt.Printf("\033[31m  Usage: rm <n>  (1–%d)\033[0m\n", len(queue))
		return queue
	}
	removed := queue[n-1].Text
	queue = append(queue[:n-1], queue[n:]...)
	fmt.Printf("\033[33m  Removed [%d] %s (queue: %d item(s))\033[0m\n", n, removed, len(queue))
	return queue
}
