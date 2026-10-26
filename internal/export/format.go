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

// line is one message in a jsonl or json file.
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
	switch w.format {
	case "jsonl", "json":
		return w.jsonEntry(e, name, fromMe, t)
	default:
		return w.textEntry(e, name, t)
	}
}

// jsonEntry writes one message as JSON: a bare object per line for jsonl, or
// an element of one array for json.
func (w *writer) jsonEntry(e *vault.Entry, name string, fromMe bool, t time.Time) error {
	b, err := json.Marshal(line{
		ID: e.ID, Time: t.Format(time.RFC3339), TS: e.Time, Sender: e.Sender, SenderName: name,
		FromMe: fromMe, Kind: e.Kind, Text: e.Text, Data: e.Data, Recalled: e.Recalled,
	})
	if err != nil {
		return err
	}
	if w.format == "jsonl" {
		w.w.Write(b)
		return w.w.WriteByte('\n')
	}
	if w.n == 1 {
		w.w.WriteString("[\n  ")
	} else {
		w.w.WriteString(",\n  ")
	}
	_, err = w.w.Write(b)
	return err
}

// textEntry writes one message for a person to read: Markdown (md) or plain
// text (txt). The two share a layout and differ only in markup.
func (w *writer) textEntry(e *vault.Entry, name string, t time.Time) error {
	md := w.format == "md"
	if w.n == 1 {
		if md {
			fmt.Fprintf(w.w, "# %s · %s\n", w.chat.Name(), t.Format("2006-01"))
		} else {
			fmt.Fprintf(w.w, "%s · %s\n", w.chat.Name(), t.Format("2006-01"))
		}
	}
	if day := t.Format("2006-01-02 Mon"); day != w.day {
		w.day = day
		if md {
			fmt.Fprintf(w.w, "\n## %s\n\n", day)
		} else {
			fmt.Fprintf(w.w, "\n%s\n\n", day)
		}
	}
	who := name
	if who == "" {
		who = "?"
	}
	if e.Kind == "system" || e.Kind == "recall" || e.Kind == "pat" {
		if md {
			fmt.Fprintf(w.w, "- %s _%s_\n", t.Format("15:04"), inline(e.Text))
		} else {
			fmt.Fprintf(w.w, "%s %s\n", t.Format("15:04"), inline(e.Text))
		}
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
		if md {
			mark = " ~~(已撤回)~~"
		} else {
			mark = " (已撤回)"
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if md {
		fmt.Fprintf(w.w, "- %s **%s**%s: %s\n", t.Format("15:04"), inline(who), mark, lines[0])
	} else {
		fmt.Fprintf(w.w, "%s %s%s: %s\n", t.Format("15:04"), who, mark, lines[0])
	}
	for _, l := range lines[1:] {
		fmt.Fprintf(w.w, "  %s\n", l)
	}
	return nil
}

func inline(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func (w *writer) close() error {
	if w.format == "json" {
		if w.n == 0 {
			w.w.WriteString("[]\n")
		} else {
			w.w.WriteString("\n]\n")
		}
	}
	return w.w.Flush()
}
