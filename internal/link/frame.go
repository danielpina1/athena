// Package link is the wire format of the dashboard link: protobuf Frames, bounded and strict.
package link

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

// MaxFrame is the largest Frame, encoded, either way.
const MaxFrame = 1 << 20

// ErrInvalid is a Frame that must be refused: too big, carrying unknown fields, or with no body.
var ErrInvalid = errors.New("invalid frame")

// Encode marshals f, refusing a Frame Decode would refuse.
func Encode(f *athenav1.Frame) ([]byte, error) {
	if f.GetBody() == nil {
		return nil, fmt.Errorf("%w: no body", ErrInvalid)
	}
	b, err := proto.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFrame {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrInvalid, len(b), MaxFrame)
	}
	return b, nil
}

// Decode unmarshals one Frame. Unknown fields at any depth are refused, not skipped: a peer that speaks a newer
// protocol must say so in Hello.protocol, not slip fields past this one.
func Decode(b []byte) (*athenav1.Frame, error) {
	if len(b) > MaxFrame {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrInvalid, len(b), MaxFrame)
	}
	f := &athenav1.Frame{}
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if f.GetBody() == nil {
		return nil, fmt.Errorf("%w: no body", ErrInvalid)
	}
	if hasUnknown(f.ProtoReflect()) {
		return nil, fmt.Errorf("%w: unknown fields", ErrInvalid)
	}
	return f, nil
}

// hasUnknown reports whether m or any message inside it carries unknown fields.
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					found = hasUnknown(mv.Message())
					return !found
				})
			}
		case fd.Message() == nil:
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len() && !found; i++ {
				found = hasUnknown(l.Get(i).Message())
			}
		default:
			found = hasUnknown(v.Message())
		}
		return !found
	})
	return found
}
