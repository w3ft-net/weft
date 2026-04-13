package store

import (
	"crypto/rand"
	"fmt"
	"time"
)

// DeviceCode represents a pending CLI authorization request.
type DeviceCode struct {
	ID         uint   `gorm:"primaryKey"`
	DeviceCode string `gorm:"uniqueIndex;not null"` // random, used by CLI to poll
	UserCode   string `gorm:"uniqueIndex;not null"` // short, displayed to user
	UserID     *string                               // set when approved
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// CreateDeviceCode generates a new device authorization request.
func (s *Store) CreateDeviceCode() (*DeviceCode, error) {
	deviceBytes := make([]byte, 16)
	if _, err := rand.Read(deviceBytes); err != nil {
		return nil, err
	}
	userBytes := make([]byte, 3)
	if _, err := rand.Read(userBytes); err != nil {
		return nil, err
	}

	dc := DeviceCode{
		DeviceCode: fmt.Sprintf("%x", deviceBytes),
		UserCode:   fmt.Sprintf("%X-%X", userBytes[:2], userBytes[2:]),
		ExpiresAt:  time.Now().Add(10 * time.Minute),
		CreatedAt:  time.Now(),
	}
	if err := s.db.Create(&dc).Error; err != nil {
		return nil, err
	}
	return &dc, nil
}

// GetDeviceCodeByUserCode finds a pending (unapproved, unexpired) device code.
func (s *Store) GetDeviceCodeByUserCode(userCode string) (*DeviceCode, error) {
	var dc DeviceCode
	err := s.db.Where("user_code = ? AND user_id IS NULL AND expires_at > ?",
		userCode, time.Now()).First(&dc).Error
	if err != nil {
		return nil, err
	}
	return &dc, nil
}

// ApproveDeviceCode marks a device code as approved by a user.
func (s *Store) ApproveDeviceCode(userCode, userID string) error {
	result := s.db.Model(&DeviceCode{}).
		Where("user_code = ? AND user_id IS NULL AND expires_at > ?", userCode, time.Now()).
		Update("user_id", userID)
	if result.RowsAffected == 0 {
		return fmt.Errorf("device code not found or expired")
	}
	return result.Error
}

// PollDeviceCode checks if a device code has been approved.
// Returns the user ID if approved, empty string if still pending, error if expired/invalid.
func (s *Store) PollDeviceCode(deviceCode string) (string, error) {
	var dc DeviceCode
	err := s.db.Where("device_code = ?", deviceCode).First(&dc).Error
	if err != nil {
		return "", fmt.Errorf("invalid device code")
	}
	if time.Now().After(dc.ExpiresAt) {
		return "", fmt.Errorf("device code expired")
	}
	if dc.UserID == nil {
		return "", nil // still pending
	}
	return *dc.UserID, nil
}
