// Package ids generates prefixed, time-sortable identifiers such as tsk_01jc7q4zp3....
package ids

import (
	"strings"

	"github.com/oklog/ulid/v2"
)

// New returns prefix + "_" + a lowercase ULID. ULIDs sort by creation time,
// which keeps cursor pagination on the primary key simple.
func New(prefix string) string {
	return prefix + "_" + strings.ToLower(ulid.Make().String())
}
