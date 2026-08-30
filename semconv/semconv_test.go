package semconv_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/gaarutyunov/goga/semconv"
)

// TestKeysAreStable pins the wire names.
//
// This is the one place in goga where an attribute key is written as a string
// literal, and it is written here precisely so that it is not written anywhere
// else: a dashboard, an alert or a stored query is built on these strings, so
// renaming one has to break a test rather than quietly break a customer's
// graphs.
func TestKeysAreStable(t *testing.T) {
	assert.Equal(t, "service.name", string(semconv.ServiceNameKey))
	assert.Equal(t, "service.version", string(semconv.ServiceVersionKey))
	assert.Equal(t, "error.type", string(semconv.ErrorTypeKey))
	assert.Equal(t, "goga.module", string(semconv.ModuleKey))
	assert.Equal(t, "goga.operation", string(semconv.OperationKey))

	assert.Equal(t, "goga.operation.duration", semconv.OperationDurationName)
	assert.Equal(t, "s", semconv.OperationDurationUnit)
	assert.Equal(t, "goga.operation.errors", semconv.OperationErrorsName)
}

func TestErrorTypeUsesTheConcreteType(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"a sentinel": {err: errors.New("boom"), want: "*errors.errorString"},
		"a typed error": {
			err:  &fs.PathError{Op: "open", Path: "/etc/secrets", Err: fs.ErrNotExist},
			want: "*fs.PathError",
		},
		"nil": {err: nil, want: "_OTHER"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := semconv.ErrorType(tt.err)

			assert.Equal(t, semconv.ErrorTypeKey, got.Key)
			assert.Equal(t, tt.want, got.Value.AsString())
		})
	}
}

// TestErrorTypeDoesNotLeakTheMessage is the reason the type is used rather than
// the message: an error string routinely carries a DSN, a row count or a
// request id, and an attribute whose value varies per occurrence multiplies the
// cardinality of every series it is attached to.
func TestErrorTypeDoesNotLeakTheMessage(t *testing.T) {
	err := &fs.PathError{Op: "open", Path: "/etc/very-secret-dsn", Err: fs.ErrNotExist}

	assert.NotContains(t, semconv.ErrorType(err).Value.AsString(), "very-secret-dsn")
}
