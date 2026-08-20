package proxy

import "github.com/google/uuid"

func randomSuffix() string {
	return uuid.NewString()
}
