// Package parse turns a message as stored by WeChat into a kind, a readable
// line of text, and kind-specific fields. Its output lives in the vault's
// derived `parsed` table; bump Version whenever the output changes, and the
// next sync re-parses everything and rebuilds the exports.
package parse

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Version of the parser's output.
const Version = 1

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var decoder = sync.OnceValue(func() *zstd.Decoder {
	d, _ := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderConcurrency(0))
	return d
})

// Decode returns the content as text, decompressing zstd.
func Decode(raw []byte) string {
	if bytes.HasPrefix(raw, zstdMagic) {
		out, err := decoder().DecodeAll(raw, nil)
		if err != nil {
			return ""
		}
		raw = out
	}
	return strings.ToValidUTF8(string(raw), "�")
}

// Result is one parsed message.
type Result struct {
	Kind string         `json:"kind"`
	Text string         `json:"text"`
	Data map[string]any `json:"data,omitempty"`
	// Sender is the sender found in a group message's "<username>:\n"
	// prefix, for rows where the shard did not name one.
	Sender string `json:"-"`
}

// Message parses one message. typ and subtype are the two halves of
// WeChat's local_type; group says the chat is a group, where the content
// starts with "<sender>:\n".
func Message(typ, subtype int64, raw []byte, group bool) Result {
	s := Decode(raw)
	var sender string
	if group {
		if i := strings.Index(s, ":\n"); i > 0 && i < 80 && !strings.ContainsAny(s[:i], " <\n") {
			sender, s = s[:i], s[i+2:]
		}
	}
	r := byType(typ, subtype, s)
	r.Sender = sender
	return r
}

func byType(typ, subtype int64, s string) Result {
	switch typ {
	case 1:
		return Result{Kind: "text", Text: s}
	case 3:
		m := parseXML(s)
		return Result{Kind: "image", Text: "[图片]", Data: compact(map[string]any{"md5": m.Img.MD5, "length": atoi(m.Img.Length)})}
	case 34:
		m := parseXML(s)
		ms := atoi(m.Voice.Length)
		return Result{Kind: "voice", Text: fmt.Sprintf("[语音 %ds]", (ms+500)/1000), Data: compact(map[string]any{"ms": ms})}
	case 43, 62:
		m := parseXML(s)
		return Result{Kind: "video", Text: "[视频]", Data: compact(map[string]any{"md5": m.Video.MD5, "seconds": atoi(m.Video.PlayLength), "length": atoi(m.Video.Length)})}
	case 47:
		m := parseXML(s)
		return Result{Kind: "sticker", Text: "[表情]", Data: compact(map[string]any{"md5": m.Emoji.MD5, "url": m.Emoji.CDNURL})}
	case 42:
		m := parseXML(s)
		return Result{Kind: "card", Text: "[名片] " + m.Nickname, Data: compact(map[string]any{"username": m.Username, "nickname": m.Nickname})}
	case 48:
		m := parseXML(s)
		l := m.Location
		name := firstOf(l.POIName, l.Label)
		return Result{Kind: "location", Text: "[位置] " + name, Data: compact(map[string]any{
			"label": l.Label, "poi": l.POIName, "lat": atof(l.X), "lng": atof(l.Y),
		})}
	case 49:
		return appMsg(subtype, s)
	case 50:
		return Result{Kind: "call", Text: "[通话] " + strings.TrimSpace(between(s, "<msg><![CDATA[", "]]></msg>"))}
	case 10000, 10002:
		return system(s)
	}
	return Result{Kind: "other", Text: fmt.Sprintf("[type %d/%d]", typ, subtype), Data: map[string]any{"raw": truncate(s, 2000)}}
}

func appMsg(subtype int64, s string) Result {
	m := parseXML(s)
	a := m.AppMsg
	if subtype == 0 {
		subtype = int64(atoi(a.Type))
	}
	data := compact(map[string]any{"type": subtype, "title": a.Title, "des": a.Des, "url": a.URL})
	switch subtype {
	case 57:
		ref := a.Refer
		quoted := ref.Content
		if strings.HasPrefix(strings.TrimSpace(quoted), "<") {
			quoted = byType(int64(atoi(ref.Type)), 0, quoted).Text
		}
		data["refer"] = compact(map[string]any{
			"server_id": ref.SvrID, "sender": ref.FromUser, "name": ref.DisplayName, "text": truncate(quoted, 500),
		})
		return Result{Kind: "quote", Text: a.Title, Data: data}
	case 5, 4, 3, 76, 87:
		return Result{Kind: "link", Text: "[链接] " + a.Title, Data: data}
	case 6:
		data["size"] = atoi(a.Attach.TotalLen)
		data["ext"] = a.Attach.FileExt
		data["md5"] = a.MD5
		return Result{Kind: "file", Text: "[文件] " + a.Title, Data: compact(data)}
	case 19, 40:
		return Result{Kind: "forward", Text: "[聊天记录] " + a.Title + "\n" + a.Des, Data: data}
	case 33, 36:
		return Result{Kind: "miniprogram", Text: "[小程序] " + firstOf(a.SourceDisplayName, "") + " " + a.Title, Data: data}
	case 51, 63:
		return Result{Kind: "channels", Text: "[视频号] " + firstOf(a.Finder.Desc, a.Title), Data: data}
	case 2000:
		data["fee"] = a.Pay.FeeDesc
		data["pay_subtype"] = atoi(a.Pay.PaySubtype)
		return Result{Kind: "transfer", Text: "[转账] " + firstOf(a.Pay.FeeDesc, a.Des), Data: compact(data)}
	case 2001:
		return Result{Kind: "redpacket", Text: "[红包] " + firstOf(a.Pay.SenderTitle, a.Title), Data: data}
	case 62:
		return Result{Kind: "pat", Text: a.Title, Data: data}
	case 53:
		return Result{Kind: "solitaire", Text: a.Title, Data: data}
	case 8:
		return Result{Kind: "sticker", Text: "[表情] " + a.Title, Data: data}
	}
	return Result{Kind: "app", Text: strings.TrimSpace(fmt.Sprintf("[app %d] %s", subtype, a.Title)), Data: data}
}

func system(s string) Result {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<") {
		return Result{Kind: "system", Text: t}
	}
	m := parseXML(t)
	if m.XMLName.Local == "sysmsg" && m.SysType == "revokemsg" {
		return Result{Kind: "recall", Text: strings.TrimSpace(m.Revoke.Content)}
	}
	if m.XMLName.Local == "sysmsg" && m.SysType == "pat" {
		return Result{Kind: "pat", Text: "[拍一拍]"}
	}
	return Result{Kind: "system", Text: truncate(stripTags(t), 500)}
}

// msgXML covers the fields used above, from all message XML shapes at once:
// encoding/xml ignores what is not there.
type msgXML struct {
	XMLName  xml.Name
	SysType  string `xml:"type,attr"`
	Username string `xml:"username,attr"`
	Nickname string `xml:"nickname,attr"`
	Img      struct {
		MD5    string `xml:"md5,attr"`
		Length string `xml:"length,attr"`
	} `xml:"img"`
	Voice struct {
		Length string `xml:"voicelength,attr"`
	} `xml:"voicemsg"`
	Video struct {
		MD5        string `xml:"md5,attr"`
		Length     string `xml:"length,attr"`
		PlayLength string `xml:"playlength,attr"`
	} `xml:"videomsg"`
	Emoji struct {
		MD5    string `xml:"md5,attr"`
		CDNURL string `xml:"cdnurl,attr"`
	} `xml:"emoji"`
	Location struct {
		X       string `xml:"x,attr"`
		Y       string `xml:"y,attr"`
		Label   string `xml:"label,attr"`
		POIName string `xml:"poiname,attr"`
	} `xml:"location"`
	AppMsg struct {
		Title  string `xml:"title"`
		Des    string `xml:"des"`
		Type   string `xml:"type"`
		URL    string `xml:"url"`
		MD5    string `xml:"md5"`
		Attach struct {
			TotalLen string `xml:"totallen"`
			FileExt  string `xml:"fileext"`
		} `xml:"appattach"`
		SourceDisplayName string `xml:"sourcedisplayname"`
		Refer             struct {
			Type        string `xml:"type"`
			SvrID       string `xml:"svrid"`
			FromUser    string `xml:"chatusr"`
			DisplayName string `xml:"displayname"`
			Content     string `xml:"content"`
		} `xml:"refermsg"`
		Pay struct {
			FeeDesc     string `xml:"feedesc"`
			PaySubtype  string `xml:"paysubtype"`
			SenderTitle string `xml:"sendertitle"`
		} `xml:"wcpayinfo"`
		Finder struct {
			Desc string `xml:"desc"`
		} `xml:"finderFeed"`
	} `xml:"appmsg"`
	Revoke struct {
		Content string `xml:"content"`
	} `xml:"revokemsg"`
}

func parseXML(s string) *msgXML {
	m := &msgXML{}
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	_ = d.Decode(m)
	m.AppMsg.Title = strings.TrimSpace(m.AppMsg.Title)
	m.AppMsg.Des = strings.TrimSpace(m.AppMsg.Des)
	return m
}

func stripTags(s string) string {
	var b strings.Builder
	d := xml.NewDecoder(strings.NewReader(s))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok {
			if t := strings.TrimSpace(string(cd)); t != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func firstOf(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// compact drops empty strings and zero numbers.
func compact(m map[string]any) map[string]any {
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if x == "" {
				delete(m, k)
			}
		case int:
			if x == 0 {
				delete(m, k)
			}
		case int64:
			if x == 0 {
				delete(m, k)
			}
		case float64:
			if x == 0 {
				delete(m, k)
			}
		}
	}
	return m
}
