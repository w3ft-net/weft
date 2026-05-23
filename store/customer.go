package store

import "gorm.io/gorm"

type Customer struct {
	gorm.Model
	Name        string       `gorm:"uniqueIndex;not null"`
	DisplayName string       // human-readable org/team name (editable)
	Namespaces  []Namespace  `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	AWSAccounts []AWSAccount `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	AgentTokens []AgentToken `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
}

// Namespace is a grouping of log sources owned by a customer.
// e.g., "weft" (containing coordinator, shard, api sub-apps).
// Agent tokens and log ingestion reference namespaces via "namespace/subapp" app names.
type Namespace struct {
	gorm.Model
	CustomerID uint   `gorm:"not null;uniqueIndex:idx_ns_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_ns_customer_name"`
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

// --- Namespaces ---

// FindOrCreateNamespace returns the namespace, creating if needed.
func (s *Store) FindOrCreateNamespace(customerID uint, name string) (*Namespace, error) {
	var ns Namespace
	result := s.scoped(customerID).Where("name = ?", name).FirstOrCreate(&ns, Namespace{CustomerID: customerID, Name: name})
	return &ns, result.Error
}

// GetNamespace looks up a namespace by (customer_id, name) without creating
// one if absent. Callers validating user-supplied namespace names (e.g.
// AddSource) should use this so a typo doesn't silently spawn a new
// namespace under the customer.
func (s *Store) GetNamespace(customerID uint, name string) (*Namespace, error) {
	var ns Namespace
	if err := s.scoped(customerID).Where("name = ?", name).First(&ns).Error; err != nil {
		return nil, err
	}
	return &ns, nil
}

// ListNamespaces returns all namespaces for a customer.
func (s *Store) ListNamespaces(customerID uint) ([]Namespace, error) {
	var nss []Namespace
	err := s.scoped(customerID).Order("name").Find(&nss).Error
	return nss, err
}

// --- Backward compat aliases ---
// TODO: remove after all callers are migrated

// FindOrCreateApp is an alias for FindOrCreateNamespace.
func (s *Store) FindOrCreateApp(customerID uint, name string) (*Namespace, error) {
	return s.FindOrCreateNamespace(customerID, name)
}

// ListApps is an alias for ListNamespaces.
func (s *Store) ListApps(customerID uint) ([]Namespace, error) {
	return s.ListNamespaces(customerID)
}

