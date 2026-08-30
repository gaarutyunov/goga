package dbopen

import (
	"context"
	"database/sql/driver"
)

// connector is the minimum needed to spell sql.OpenDB's argument.
type connector interface {
	Connect(context.Context) (driver.Conn, error)
	Driver() driver.Driver
}
