package metadata

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type MaintainerID string
type ApplicationID string

// Revision tracks server mutations independently of an application's business version.
type Revision int64

type MaintainerFields struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type ApplicationFields struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Company     string `json:"company"`
	Website     string `json:"website"`
	Source      string `json:"source"`
	License     string `json:"license"`
	Description string `json:"description"`
}

// CreateApplicationInput accepts a creator only at creation; replacements use ApplicationFields.
type CreateApplicationInput struct {
	ApplicationFields
	MaintainerID MaintainerID `json:"maintainer_id"`
}

type Maintainer struct {
	ID MaintainerID `json:"id"`
	MaintainerFields
	Revision  Revision  `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Application struct {
	ID ApplicationID `json:"id"`
	ApplicationFields
	// MaintainerID is the recorded creator, not a current responsibility assignment.
	MaintainerID MaintainerID `json:"maintainer_id"`
	Revision     Revision     `json:"revision"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

func NewUUID() string {
	var bytes [16]byte
	// crypto/rand.Read cannot return an error on supported Go platforms.
	_, _ = rand.Read(bytes[:])
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	var text [36]byte
	hex.Encode(text[0:8], bytes[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], bytes[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], bytes[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], bytes[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], bytes[10:16])
	return string(text[:])
}
