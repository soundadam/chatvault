package parse

import (
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeZstd(t *testing.T) {
	enc, _ := zstd.NewWriter(nil)
	raw := enc.EncodeAll([]byte("你好"), nil)
	if got := Decode(raw); got != "你好" {
		t.Fatalf("Decode = %q", got)
	}
	if got := Decode([]byte("plain")); got != "plain" {
		t.Fatalf("Decode = %q", got)
	}
}

func TestMessage(t *testing.T) {
	cases := []struct {
		name       string
		typ, sub   int64
		content    string
		group      bool
		kind, text string
		sender     string
	}{
		{"text", 1, 0, "hello", false, "text", "hello", ""},
		{"group text", 1, 0, "wxid_abc:\nhi there", true, "text", "hi there", "wxid_abc"},
		{"group no prefix", 1, 0, "a: b c:\nd", true, "text", "a: b c:\nd", ""},
		{"image", 3, 0, `<msg><img md5="abc" length="12"/></msg>`, false, "image", "[图片]", ""},
		{"voice", 34, 0, `<msg><voicemsg voicelength="4600"/></msg>`, false, "voice", "[语音 5s]", ""},
		{"video", 43, 0, `<msg><videomsg playlength="9" md5="x"/></msg>`, false, "video", "[视频]", ""},
		{"card", 42, 0, `<msg username="wxid_q" nickname="Bob"/>`, false, "card", "[名片] Bob", ""},
		{"location", 48, 0, `<msg><location x="32.1" y="118.9" label="南京" poiname="南大"/></msg>`, false, "location", "[位置] 南大", ""},
		{"call", 50, 0, `<voipmsg type="VoIPBubbleMsg"><VoIPBubbleMsg><msg><![CDATA[通话时长 01:24]]></msg></VoIPBubbleMsg></voipmsg>`, false, "call", "[通话] 通话时长 01:24", ""},
		{"quote", 49, 57, `<msg><appmsg><title>好的</title><type>57</type><refermsg><type>1</type><svrid>9</svrid><displayname>A</displayname><content>原文</content></refermsg></appmsg></msg>`, false, "quote", "好的", ""},
		{"link", 49, 5, `<msg><appmsg><title>标题</title><url>https://x</url></appmsg></msg>`, false, "link", "[链接] 标题", ""},
		{"file", 49, 6, `<msg><appmsg><title>a.pdf</title><appattach><totallen>10</totallen><fileext>pdf</fileext></appattach></appmsg></msg>`, false, "file", "[文件] a.pdf", ""},
		{"transfer", 49, 2000, `<msg><appmsg><title>微信转账</title><wcpayinfo><feedesc>￥10.00</feedesc></wcpayinfo></appmsg></msg>`, false, "transfer", "[转账] ￥10.00", ""},
		{"recall", 10000, 0, `<sysmsg type="revokemsg"><revokemsg><content>"A" 撤回了一条消息</content></revokemsg></sysmsg>`, false, "recall", `"A" 撤回了一条消息`, ""},
		{"system plain", 10000, 0, "你已添加了A", false, "system", "你已添加了A", ""},
		{"unknown", 99, 0, "x", false, "other", "[type 99/0]", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Message(c.typ, c.sub, []byte(c.content), c.group)
			if r.Kind != c.kind || r.Text != c.text || r.Sender != c.sender {
				t.Fatalf("got kind=%q text=%q sender=%q", r.Kind, r.Text, r.Sender)
			}
		})
	}
}

func TestQuoteData(t *testing.T) {
	r := Message(49, 57, []byte(`<msg><appmsg><title>ok</title><refermsg><type>1</type><svrid>9</svrid><displayname>A</displayname><content>原文</content></refermsg></appmsg></msg>`), false)
	ref, _ := r.Data["refer"].(map[string]any)
	if ref["text"] != "原文" || ref["name"] != "A" || ref["server_id"] != "9" {
		t.Fatalf("refer = %v", ref)
	}
}
