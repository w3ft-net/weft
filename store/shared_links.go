package store

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// SharedLink is what a `clipboard` short link points at: a /bin script, the
// time range to open it with, and its arguments.
type SharedLink struct {
	OrgID           OrgID  `gorm:"primaryKey"`
	Slug            string `gorm:"primaryKey"`
	Bin             string `gorm:"not null"`
	RangeSpec       string `gorm:"column:range_spec;not null"`
	Args            string `gorm:"not null"`
	CreatedByUserID *string
	CreatedAt       time.Time
}

const (
	base62          = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sharedSlugLen   = 10
	sharedSlugTries = 5
)

// sharedSlug is the short name for a link: sha256 of its org and payload,
// reduced to sharedSlugLen base62 digits. attempt rehashes after a collision.
func sharedSlug(orgID OrgID, bin, rangeSpec, args string, attempt int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%d", orgID, bin, rangeSpec, args, attempt)))
	n := binary.BigEndian.Uint64(h[:8])
	out := make([]byte, sharedSlugLen)
	for i := range out {
		out[i] = base62[n%62]
		n /= 62
	}
	return string(out)
}

// SaveSharedLink records a link and returns its slug. Sharing the same
// script, range and args again returns the same slug and adds no row.
func (s *Store) SaveSharedLink(orgID OrgID, userID, bin, rangeSpec, args string) (string, error) {
	for attempt := 0; attempt < sharedSlugTries; attempt++ {
		slug := sharedSlug(orgID, bin, rangeSpec, args, attempt)
		row := &SharedLink{OrgID: orgID, Slug: slug, Bin: bin, RangeSpec: rangeSpec, Args: args}
		if userID != "" {
			row.CreatedByUserID = &userID
		}
		if err := s.scoped(orgID).Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
			return "", err
		}
		got, err := s.GetSharedLink(orgID, slug)
		if err != nil {
			return "", err
		}
		if got.Bin == bin && got.RangeSpec == rangeSpec && got.Args == args {
			return slug, nil
		}
		// A different payload already owns this slug (a 1-in-6e17 hash
		// collision); try the next one.
	}
	return "", fmt.Errorf("could not allocate a short link")
}

// GetSharedLink resolves a slug within one org.
func (s *Store) GetSharedLink(orgID OrgID, slug string) (*SharedLink, error) {
	var l SharedLink
	if err := s.scoped(orgID).Where("slug = ?", slug).First(&l).Error; err != nil {
		return nil, err
	}
	return &l, nil
}
