package bus

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestMessagesWholeRowBounds(t *testing.T) {
	rows := []busproto.MessagesItem{}
	for _, id := range []string{"c", "b", "a"} {
		rows = append(rows, busproto.MessagesItem{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: id, Sent: time.Unix(1, 0), Body: strings.Repeat("界", 300)}}})
	}
	out, err := boundMessages(busproto.MessagesResponse{Kind: "messages", Thread: "t", Limit: 20}, rows, 2400)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if len(raw)+1 > 2400 || !out.More || len(out.Messages) != 1 || out.Next != "1970-01-01T00:00:01Z|c" || out.Messages[0].Body != rows[0].Body {
		t.Fatalf("whole row boundary: %+v %s", out, raw)
	}
	if _, err := boundMessages(busproto.MessagesResponse{Thread: "t", Limit: 20}, rows, 200); err == nil {
		t.Fatal("oversized first row needs actionable error")
	}
}
