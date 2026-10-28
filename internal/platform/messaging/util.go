package messaging

import (
	"errors"

	"github.com/google/uuid"
)

func parseID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

func isPoison(err error) bool { return errors.Is(err, ErrPoison) }
