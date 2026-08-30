package database

import (
	"context"
	"database/sql/driver"
)

// driverConnector is the minimum this fixture needs to spell sql.OpenDB's
// argument without depending on anything outside the standard library.
type driverConnector interface {
	Connect(context.Context) (driver.Conn, error)
	Driver() driver.Driver
}
