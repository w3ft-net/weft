package store

import "gorm.io/gorm"

type Customer struct {
	gorm.Model
	Name        string       `gorm:"uniqueIndex;not null"`
	DisplayName string       // human-readable org/team name (editable)
	Apps        []App        `gorm:"foreignKey:CustomerID"`
	Accounts    []Account    `gorm:"foreignKey:CustomerID"`
	AgentTokens []AgentToken `gorm:"foreignKey:CustomerID"`
}

// App is a named log source owned by a customer. Tokens and ingestion
// configuration (CloudWatch, Kinesis, agent) hang off apps.
type App struct {
	gorm.Model
	CustomerID uint   `gorm:"not null;uniqueIndex:idx_app_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_app_customer_name"`
}

// FindOrCreateCustomer returns the customer, creating if needed.
func (s *Store) FindOrCreateCustomer(name string) (*Customer, error) {
	var c Customer
	result := s.db.Where("name = ?", name).FirstOrCreate(&c, Customer{Name: name})
	return &c, result.Error
}

// GetCustomerByID returns a customer by primary key.
func (s *Store) GetCustomerByID(id uint) (*Customer, error) {
	var c Customer
	if err := s.db.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// GetCustomerByName returns a customer by name.
func (s *Store) GetCustomerByName(name string) (*Customer, error) {
	var c Customer
	if err := s.db.Where("name = ?", name).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateCustomerDisplayName sets the human-readable team/org name.
func (s *Store) UpdateCustomerDisplayName(name, displayName string) error {
	return s.db.Model(&Customer{}).Where("name = ?", name).Update("display_name", displayName).Error
}

// --- Apps ---

// FindOrCreateApp returns the app, creating if needed.
func (s *Store) FindOrCreateApp(customerID uint, name string) (*App, error) {
	var a App
	result := s.db.Where("customer_id = ? AND name = ?", customerID, name).FirstOrCreate(&a, App{CustomerID: customerID, Name: name})
	return &a, result.Error
}

// ListApps returns all apps for a customer.
func (s *Store) ListApps(customerID uint) ([]App, error) {
	var apps []App
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&apps).Error
	return apps, err
}

// AllAppNames returns distinct app names across all customers.
func (s *Store) AllAppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&App{}).Distinct().Pluck("name", &names).Error
	return names, err
}

// DeleteApp removes an app.
func (s *Store) DeleteApp(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&App{}).Error
}
