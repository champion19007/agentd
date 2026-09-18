package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/champion19007/agentd/internal/core/domain"
)

// Structured domain values are stored as JSON rather than shredded into
// columns. They are read and written whole and never queried into, so columns
// would buy nothing and would couple the schema to shapes that are still
// settling.
//
// Intents need a hand-written decoder because Intent is an interface and
// encoding/json cannot know which concrete type a row holds. The kind is
// stored in its own column, so the decision is a switch rather than a guess.

// timeFormat is RFC3339 with nanoseconds, in UTC. It sorts lexicographically
// in the same order it sorts chronologically, which is what lets retention
// sweeps and "most recent first" use plain string comparison in SQL.
const timeFormat = time.RFC3339Nano

// codec is a placeholder for per-store encoding state. It exists so that
// compression settings can become configurable without changing signatures.
type codec struct{}

// --- time -------------------------------------------------------------------

// encodeTime renders an instant for storage. The zero time becomes NULL, so
// that "this stage was never reached" survives a round trip instead of
// arriving back as the year 1.
func encodeTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(timeFormat), Valid: true}
}

// mustEncodeTime renders an instant for a NOT NULL column.
func mustEncodeTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// decodeTime reads an instant back. NULL becomes the zero time.
func decodeTime(v sql.NullString) (time.Time, error) {
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeFormat, v.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlite: unreadable instant %q: %w", v.String, err)
	}
	return t.UTC(), nil
}

// --- JSON -------------------------------------------------------------------

func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlite: encoding %T: %w", v, err)
	}
	return string(b), nil
}

func decodeJSON[T any](s string, into *T) error {
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), into); err != nil {
		return fmt.Errorf("sqlite: decoding %T: %w", into, err)
	}
	return nil
}

// --- intents ----------------------------------------------------------------

// decodeIntent rebuilds an Intent from its kind and body. The kind lives in
// its own column precisely so this is a lookup rather than an inference.
func decodeIntent(kind, body string) (domain.Intent, error) {
	switch domain.IntentKind(kind) {
	case domain.IntentScalar:
		var in domain.ScalarIntent
		if err := decodeJSON(body, &in); err != nil {
			return nil, err
		}
		return in, nil
	case domain.IntentRecord:
		var in domain.RecordIntent
		if err := decodeJSON(body, &in); err != nil {
			return nil, err
		}
		return in, nil
	case domain.IntentCollection:
		var in domain.CollectionIntent
		if err := decodeJSON(body, &in); err != nil {
			return nil, err
		}
		return in, nil
	default:
		return nil, fmt.Errorf("sqlite: stored intent has unknown kind %q", kind)
	}
}

// --- failures and results ---------------------------------------------------

// encodeFailure stores a failure, or NULL when there is none.
func encodeFailure(f *domain.Failure) (sql.NullString, error) {
	if f == nil {
		return sql.NullString{}, nil
	}
	s, err := encodeJSON(f)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: s, Valid: true}, nil
}

func decodeFailure(v sql.NullString) (*domain.Failure, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	var f domain.Failure
	if err := decodeJSON(v.String, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// encodeExtraction stores a run's result, or NULL for a run that never got one.
func encodeExtraction(e domain.Extraction) (sql.NullString, error) {
	if e.Kind == "" {
		return sql.NullString{}, nil
	}
	s, err := encodeJSON(e)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: s, Valid: true}, nil
}

func decodeExtraction(v sql.NullString) (domain.Extraction, error) {
	if !v.Valid || v.String == "" {
		return domain.Extraction{}, nil
	}
	var e domain.Extraction
	if err := decodeJSON(v.String, &e); err != nil {
		return domain.Extraction{}, err
	}
	return e, nil
}

// --- snapshot bodies --------------------------------------------------------

// Captures are the bulk of the database and are highly compressible: they are
// mostly the same page over and over, and the reason to keep several is to
// compare them. zstd at its default level is the right trade -- fast enough
// that it does not slow a run down, and effective enough that keeping ten
// captures of a large page is unremarkable.
//
// The compression algorithm is stored per row rather than assumed, so a future
// change of codec does not require rewriting every existing body.

const (
	// compressionZstd marks a body compressed with zstd.
	compressionZstd = "zstd"
	// compressionNone marks a body stored as-is, used when compression would
	// make it bigger.
	compressionNone = "none"
)

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	encoderErr  error

	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

// zstdEncoder returns the shared encoder. It is safe for concurrent use, and
// shared because building one allocates buffers worth reusing.
func zstdEncoder() (*zstd.Encoder, error) {
	encoderOnce.Do(func() {
		encoder, encoderErr = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
	})
	return encoder, encoderErr
}

func zstdDecoder() (*zstd.Decoder, error) {
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
	return decoder, decoderErr
}

// compressBody compresses a capture, falling back to storing it uncompressed
// when compression does not help. Small or already-compressed payloads can
// come out larger, and storing the larger one would be silly.
func compressBody(body []byte) ([]byte, string, error) {
	enc, err := zstdEncoder()
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: preparing compression: %w", err)
	}
	packed := enc.EncodeAll(body, nil)
	if len(packed) >= len(body) {
		stored := make([]byte, len(body))
		copy(stored, body)
		return stored, compressionNone, nil
	}
	return packed, compressionZstd, nil
}

// decompressBody restores a capture.
func decompressBody(stored []byte, compression string) ([]byte, error) {
	switch compression {
	case compressionNone:
		out := make([]byte, len(stored))
		copy(out, stored)
		return out, nil
	case compressionZstd:
		dec, err := zstdDecoder()
		if err != nil {
			return nil, fmt.Errorf("sqlite: preparing decompression: %w", err)
		}
		out, err := dec.DecodeAll(stored, nil)
		if err != nil {
			return nil, fmt.Errorf("sqlite: unpacking capture: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("sqlite: capture stored with unknown compression %q", compression)
	}
}

// --- small helpers ----------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
