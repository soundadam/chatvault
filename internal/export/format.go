package export

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/soundadam/chatvault/internal/vault"
)

type writer struct {
	w      *bufio.Writer
	format string
	chat   *vault.Chat
	day    string
	n      int
}

func newWriter(w io.Writer, format string, c *vault.Chat) *writer {
	return &writer{w: bufio.NewWriterSize(w, 64<<10), format: format, chat: c}
}

// line is one message in a jsonl file.
type line struct {
	ID         int64           `json:"id"`
	Time       string          `json:"time"`
	TS         int64           `json:"ts"`
	Sender     string          `json:"sender,omitempty"`
	SenderName string          `json:"sender_name,omitempty"`
	FromMe     bool            `json:"from_me,omitempty"`
	Kind       string          `json:"kind"`
	Text       string          `json:"text"`
	Data       json.RawMessage `json:"data,omitempty"`
	Recalled   bool            `json:"recalled,omitempty"`
}

func (w *writer) entry(e *vault.Entry, name string, fromMe bool) error {
	t := time.Unix(e.Time, 0)
	w.n++
	if w.format == "jsonl" {
		b, err := json.Marshal(line{
			ID: e.ID, Time: t.Format(time.RFC3339), TS: e.Time, Sender: e.Sender, SenderName: name,
			FromMe: fromMe, Kind: e.Kind, Text: e.Text, Data: e.Data, Recalled: e.Recalled,
		})
		if err != nil {
			return err
		}
		w.w.Write(b)
		return w.w.WriteByte('\n')
	}

	if w.n == 1 {
		fmt.Fprintf(w.w, "# %s · %s\n", w.chat.Name(), t.Format("2006-01"))
	}
	if day := t.Format("2006-01-02 Mon"); day != w.day {
		w.day = day
		fmt.Fprintf(w.w, "\n## %s\n\n", day)
	}
	who := name
	if who == "" {
		who = "?"
	}
	if e.Kind == "system" || e.Kind == "recall" || e.Kind == "pat" {
		fmt.Fprintf(w.w, "- %s _%s_\n", t.Format("15:04"), mdInline(e.Text))
		return nil
	}
	text := e.Text
	if e.Kind == "quote" {
		var d struct {
			Refer struct {
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"refer"`
		}
		if json.Unmarshal(e.Data, &d) == nil && d.Refer.Text != "" {
			text += "\n> " + d.Refer.Name + ": " + d.Refer.Text
		}
	}
	mark := ""
	if e.Recalled {
		mark = " ~~(已撤回)~~"
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	fmt.Fprintf(w.w, "- %s **%s**%s: %s\n", t.Format("15:04"), mdInline(who), mark, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(w.w, "  %s\n", l)
	}
	return nil
}

func mdInline(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func (w *writer) close() error { return w.w.Flush() }
