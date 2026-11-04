package link

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

func bodies() map[string]*athenav1.Frame {
	f := func(ch athenav1.Channel, set func(*athenav1.Frame)) *athenav1.Frame {
		fr := &athenav1.Frame{Channel: ch, Seq: 7, Ack: 3}
		set(fr)
		return fr
	}
	c := athenav1.Channel_CONTROL
	return map[string]*athenav1.Frame{
		"hello": f(c, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_Hello{Hello: &athenav1.Hello{Protocol: 1, ClientId: "c", Epoch: "e", Cursor: 5, LastSeq: 4}}
		}),
		"welcome": f(c, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_Welcome{Welcome: &athenav1.Welcome{Epoch: "e", ServerVersion: "v", HeartbeatMs: 15000, SnapshotFollows: true}}
		}),
		"ping":   f(c, func(x *athenav1.Frame) { x.Body = &athenav1.Frame_Ping{Ping: &athenav1.Ping{AtUnixMs: 1}} }),
		"pong":   f(c, func(x *athenav1.Frame) { x.Body = &athenav1.Frame_Pong{Pong: &athenav1.Pong{AtUnixMs: 1}} }),
		"credit": f(c, func(x *athenav1.Frame) { x.Body = &athenav1.Frame_Credit{Credit: &athenav1.Credit{Channel: athenav1.Channel_STREAM, Items: 256}} }),
		"error":  f(c, func(x *athenav1.Frame) { x.Body = &athenav1.Frame_Error{Error: &athenav1.Error{Code: "invalid", Message: "m"}} }),
		"resync": f(c, func(x *athenav1.Frame) { x.Body = &athenav1.Frame_Resync{Resync: &athenav1.Resync{}} }),
		"command": f(c, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_Command{Command: &athenav1.Command{Id: "1", Action: &athenav1.Command_Reply{Reply: &athenav1.Reply{TicketId: "T-1", Text: "hi"}}}}
		}),
		"command_result": f(c, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_CommandResult{CommandResult: &athenav1.CommandResult{Id: "1", Ok: true, Result: &athenav1.CommandResult_PolicyRevision{PolicyRevision: 2}}}
		}),
		"snapshot": f(athenav1.Channel_EVENTS, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_Snapshot{Snapshot: &athenav1.Snapshot{Cursor: 9, Tickets: []*athenav1.TicketSummary{{Id: "T-1", NeedsYou: true}}}}
		}),
		"delta": f(athenav1.Channel_EVENTS, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_Delta{Delta: &athenav1.Delta{Seq: 10, Change: &athenav1.Delta_Policy{Policy: &athenav1.PolicyView{Revision: 1, SettingsJson: "{}"}}}}
		}),
		"stream_open": f(athenav1.Channel_STREAM, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_StreamOpen{StreamOpen: &athenav1.StreamOpen{TicketId: "T-1", FromOffset: -1}}
		}),
		"stream_items": f(athenav1.Channel_STREAM, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_StreamItems{StreamItems: &athenav1.StreamItems{TicketId: "T-1", Items: []*athenav1.StreamItem{{Offset: 1, Kind: "say", Text: "t"}}}}
		}),
		"stream_gap": f(athenav1.Channel_STREAM, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_StreamGap{StreamGap: &athenav1.StreamGap{TicketId: "T-1", From: 1, To: 5}}
		}),
		"stream_close": f(athenav1.Channel_STREAM, func(x *athenav1.Frame) {
			x.Body = &athenav1.Frame_StreamClose{StreamClose: &athenav1.StreamClose{TicketId: "T-1"}}
		}),
	}
}

func TestFrameRoundTrip(t *testing.T) {
	all := bodies()
	if len(all) != 15 {
		t.Fatalf("covering %d body types, want 15", len(all))
	}
	for name, in := range all {
		b, err := Encode(in)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		out, err := Decode(b)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !proto.Equal(in, out) {
			t.Errorf("%s: round trip changed the frame: %v != %v", name, in, out)
		}
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	unused := protowire.AppendTag(nil, 999, protowire.VarintType)
	unused = protowire.AppendVarint(unused, 1)

	top, _ := Encode(bodies()["ping"])
	if _, err := Decode(append(top, unused...)); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown field on Frame: err = %v, want ErrInvalid", err)
	}

	// A nested Command carrying the unknown field: rebuild the Frame with the extra bytes inside the Command.
	cmd := &athenav1.Command{Id: "1", Action: &athenav1.Command_Retry{Retry: &athenav1.Retry{TicketId: "T-1"}}}
	cb, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	cb = append(cb, unused...)
	var raw []byte
	raw = protowire.AppendTag(raw, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, uint64(athenav1.Channel_CONTROL))
	raw = protowire.AppendTag(raw, 17, protowire.BytesType)
	raw = protowire.AppendBytes(raw, cb)
	if _, err := Decode(raw); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown field on nested Command: err = %v, want ErrInvalid", err)
	}

	// The same frame without the extra field is fine.
	cb, _ = proto.Marshal(cmd)
	ok := protowire.AppendTag(nil, 17, protowire.BytesType)
	ok = protowire.AppendBytes(ok, cb)
	if _, err := Decode(ok); err != nil {
		t.Errorf("clean frame: %v", err)
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	f := &athenav1.Frame{Body: &athenav1.Frame_Error{Error: &athenav1.Error{Message: strings.Repeat("x", MaxFrame)}}}
	if _, err := Encode(f); !errors.Is(err, ErrInvalid) {
		t.Errorf("Encode oversize: err = %v, want ErrInvalid", err)
	}
	b, err := proto.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
		t.Errorf("Decode oversize: err = %v, want ErrInvalid", err)
	}
}

func TestDecodeRejectsEmptyBody(t *testing.T) {
	for _, b := range [][]byte{nil, {}} {
		if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("Decode(%v): err = %v, want ErrInvalid", b, err)
		}
	}
	b, _ := proto.Marshal(&athenav1.Frame{Channel: athenav1.Channel_CONTROL, Seq: 1})
	if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
		t.Errorf("Decode(no body): err = %v, want ErrInvalid", err)
	}
	if _, err := Encode(&athenav1.Frame{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Encode(no body): err = %v, want ErrInvalid", err)
	}
}
