package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// TestSaveSlackConfig_CreatesAndUpdates pins both branches of the
// upsert: a customer with no NotifyConfig row gets one inserted; a
// subsequent call updates in place rather than inserting a duplicate.
// The unique index on customer_id would catch a regression, but
// asserting on a fresh GetNotifyConfig after each call keeps the
// failure mode legible.
func TestSaveSlackConfig_CreatesAndUpdates(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateCustomer("slack-cfg-test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert path.
	if err := db.SaveSlackConfig(cust.ID, "https://hooks.slack/A", "xoxb-aaa", "Workspace A"); err != nil {
		t.Fatalf("first SaveSlackConfig: %v", err)
	}
	nc, err := db.GetNotifyConfig(cust.ID)
	if err != nil {
		t.Fatalf("GetNotifyConfig: %v", err)
	}
	if nc.SlackWebhook != "https://hooks.slack/A" || nc.SlackBotToken != "xoxb-aaa" || nc.SlackTeamName != "Workspace A" {
		t.Errorf("after insert: %+v", nc)
	}
	if nc.ConfiguredAt == nil {
		t.Error("ConfiguredAt should be set on first save")
	}

	// Update path.
	if err := db.SaveSlackConfig(cust.ID, "https://hooks.slack/B", "xoxb-bbb", "Workspace B"); err != nil {
		t.Fatalf("second SaveSlackConfig: %v", err)
	}
	nc2, _ := db.GetNotifyConfig(cust.ID)
	if nc2.SlackWebhook != "https://hooks.slack/B" {
		t.Errorf("update didn't take: %+v", nc2)
	}
	// Same ID — confirms update-not-insert.
	if nc2.ID != nc.ID {
		t.Errorf("expected same row; got new ID %d vs %d", nc2.ID, nc.ID)
	}
}

// TestSaveSlackWebhook_LegacyPath: SaveSlackWebhook is a thin shim
// over SaveSlackConfig that leaves the bot token empty. Used by the
// pre-OAuth flow where customers paste a webhook URL directly.
func TestSaveSlackWebhook_LegacyPath(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("slack-webhook-test")

	if err := db.SaveSlackConfig(cust.ID, "https://hooks.slack/legacy", "", "Legacy WS"); err != nil {
		t.Fatalf("save slack webhook: %v", err)
	}
	nc, _ := db.GetNotifyConfig(cust.ID)
	if nc.SlackBotToken != "" {
		t.Errorf("bot token should be empty for webhook-only path, got %q", nc.SlackBotToken)
	}
	if nc.SlackWebhook != "https://hooks.slack/legacy" {
		t.Errorf("webhook = %q", nc.SlackWebhook)
	}
}

// TestDisconnectSlack clears webhook + team name while leaving the
// row in place — `nc` is keyed (uniquely) by customer_id and also
// holds notify-aliases yaml, so we don't tear down the whole row.
func TestDisconnectSlack(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("slack-disconnect-test")

	db.SaveSlackConfig(cust.ID, "https://hooks.slack/X", "xoxb-x", "X Team")
	db.SaveNotifyAliases(cust.ID, "page: oncall\n")

	if err := db.scoped(cust.ID).Model(&NotifyConfig{}).Updates(map[string]any{
		"slack_webhook":   "",
		"slack_team_name": "",
	}).Error; err != nil {
		t.Fatalf("DisconnectSlack: %v", err)
	}
	nc, _ := db.GetNotifyConfig(cust.ID)
	if nc.SlackWebhook != "" || nc.SlackTeamName != "" {
		t.Errorf("slack fields not cleared: %+v", nc)
	}
	if nc.AliasesYAML != "page: oncall\n" {
		t.Errorf("aliases should be preserved through DisconnectSlack: %q", nc.AliasesYAML)
	}
}

// TestGetNotifyConfig_NotFound returns gorm.ErrRecordNotFound for an
// un-provisioned customer. Callers (api/integrations.go, notifier)
// special-case this as "not configured" rather than treating it as
// a real error.
func TestGetNotifyConfig_NotFound(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("no-notify-test")

	_, err := db.GetNotifyConfig(cust.ID)
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("err = %v, want ErrRecordNotFound", err)
	}
}

// ── PagerDuty integrations ─────────────────────────────────────

func TestPagerDuty_CRUD(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-crud-test")

	// Empty list.
	items, err := listPagerDuty(db, cust.ID)
	if err != nil {
		t.Fatalf("ListPagerDutyIntegrations: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 integrations on fresh customer, got %d", len(items))
	}

	// Create.
	if err := savePagerDuty(db, cust.ID, "prod-oncall", "key-prod-aaa"); err != nil {
		t.Fatalf("SavePagerDutyIntegration insert: %v", err)
	}

	// Get.
	got, err := getPagerDuty(db, cust.ID, "prod-oncall")
	if err != nil {
		t.Fatalf("GetPagerDutyIntegration: %v", err)
	}
	if got.IntegrationKey != "key-prod-aaa" {
		t.Errorf("IntegrationKey = %q", got.IntegrationKey)
	}

	// Update by re-saving the same name — should update key, keep ID.
	if err := savePagerDuty(db, cust.ID, "prod-oncall", "key-prod-rotated"); err != nil {
		t.Fatalf("SavePagerDutyIntegration update: %v", err)
	}
	got2, _ := getPagerDuty(db, cust.ID, "prod-oncall")
	if got2.IntegrationKey != "key-prod-rotated" {
		t.Errorf("update didn't take: %+v", got2)
	}
	if got2.ID != got.ID {
		t.Errorf("expected same row ID; got %d vs %d", got2.ID, got.ID)
	}

	// List shows the row.
	items, _ = listPagerDuty(db, cust.ID)
	if len(items) != 1 {
		t.Errorf("len = %d", len(items))
	}
}

// TestPagerDuty_ListOrdersByName: ListPagerDutyIntegrations explicitly
// orders by name. Pin the order so UI rendering and `weft pd list`
// output stay stable.
func TestPagerDuty_ListOrdersByName(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-order-test")

	// Insert in non-alphabetic order.
	for _, n := range []string{"zulu", "alpha", "mike"} {
		if err := savePagerDuty(db, cust.ID, n, "k-"+n); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := listPagerDuty(db, cust.ID)
	if len(items) != 3 {
		t.Fatalf("len = %d", len(items))
	}
	want := []string{"alpha", "mike", "zulu"}
	for i, w := range want {
		if items[i].Name != w {
			t.Errorf("items[%d].Name = %q, want %q", i, items[i].Name, w)
		}
	}
}

// TestPagerDuty_DeleteOne removes a single named integration without
// affecting siblings. Soft-delete via gorm.Model.DeletedAt.
func TestPagerDuty_DeleteOne(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-delete-test")

	savePagerDuty(db, cust.ID, "a", "k-a")
	savePagerDuty(db, cust.ID, "b", "k-b")

	if err := db.scoped(cust.ID).Where("name = ?", "a").Delete(&PagerDutyIntegration{}).Error; err != nil {
		t.Fatalf("delete pagerduty: %v", err)
	}
	items, _ := listPagerDuty(db, cust.ID)
	if len(items) != 1 || items[0].Name != "b" {
		t.Errorf("after delete-a: %+v", items)
	}
}

// TestPagerDuty_DeleteAll clears every integration for a customer in
// one shot. Used by the disconnect-all path in `weft pd disconnect`.
func TestPagerDuty_DeleteAll(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-deleteall-test")

	for _, n := range []string{"a", "b", "c"} {
		savePagerDuty(db, cust.ID, n, "k-"+n)
	}
	if err := deleteAllPagerDuty(db, cust.ID); err != nil {
		t.Fatalf("DeleteAllPagerDutyIntegrations: %v", err)
	}
	items, _ := listPagerDuty(db, cust.ID)
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

// TestPagerDuty_DeleteAll_OtherCustomerUnaffected: the delete-all
// is scoped to one customer. Two-tenant isolation is the contract
// the api layer relies on for cross-customer safety.
func TestPagerDuty_DeleteAll_OtherCustomerUnaffected(t *testing.T) {
	db := testOpen(t)
	a, _ := db.FindOrCreateCustomer("pd-iso-a")
	b, _ := db.FindOrCreateCustomer("pd-iso-b")

	savePagerDuty(db, a.ID, "primary", "k-a")
	savePagerDuty(db, b.ID, "primary", "k-b")

	deleteAllPagerDuty(db, a.ID)

	items, _ := listPagerDuty(db, b.ID)
	if len(items) != 1 {
		t.Errorf("customer B's integrations should be untouched, got %d", len(items))
	}
}

// TestPagerDuty_RecordSend updates last_used_at + last_error after a
// dispatch attempt. Used by the notifier to surface deliverability
// state in the UI without storing every send.
func TestPagerDuty_RecordSend(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-record-test")
	savePagerDuty(db, cust.ID, "main", "k-main")

	// Success path: empty error string.
	if err := db.RecordPagerDutySend(cust.ID, "main", ""); err != nil {
		t.Fatalf("RecordPagerDutySend: %v", err)
	}
	got, _ := getPagerDuty(db, cust.ID, "main")
	if got.LastUsedAt == nil {
		t.Error("LastUsedAt should be set after RecordPagerDutySend")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}

	// Failure path: error stored.
	if err := db.RecordPagerDutySend(cust.ID, "main", "rate-limited"); err != nil {
		t.Fatalf("RecordPagerDutySend(error): %v", err)
	}
	got, _ = getPagerDuty(db, cust.ID, "main")
	if got.LastError != "rate-limited" {
		t.Errorf("LastError = %q", got.LastError)
	}
}

// TestPagerDuty_GetNotFound returns gorm.ErrRecordNotFound when the
// integration doesn't exist for the customer. The api layer maps
// this to a 404.
func TestPagerDuty_GetNotFound(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("pd-notfound-test")

	_, err := getPagerDuty(db, cust.ID, "missing")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("err = %v, want ErrRecordNotFound", err)
	}
}

// TestPagerDutyIntegration_TableName pins the GORM-overridden plural
// table name (so a future model rename doesn't silently break the
// migration's CREATE INDEX statements that reference the table).
func TestPagerDutyIntegration_TableName(t *testing.T) {
	pd := PagerDutyIntegration{}
	if pd.TableName() != "pagerduty_integrations" {
		t.Errorf("TableName = %q", pd.TableName())
	}
}

// --- helpers for PagerDuty store methods relocated out of the store package ---
//
// Production logic moved to callers outside the store package. These
// test-local helpers reproduce the original method bodies verbatim
// (parameterized on db) so the notify tests keep exercising the
// same behaviour.

// savePagerDuty mirrors the former Store.SavePagerDutyIntegration.
func savePagerDuty(db *Store, customerID uint, name, key string) error {
	var item PagerDutyIntegration
	result := db.scoped(customerID).Where("name = ?", name).First(&item)
	if result.Error == gorm.ErrRecordNotFound {
		return db.scoped(customerID).Create(&PagerDutyIntegration{
			CustomerID:     customerID,
			Name:           name,
			IntegrationKey: key,
		}).Error
	}
	return db.scoped(customerID).Model(&item).Update("integration_key", key).Error
}

// getPagerDuty mirrors the former Store.GetPagerDutyIntegration.
func getPagerDuty(db *Store, customerID uint, name string) (*PagerDutyIntegration, error) {
	var item PagerDutyIntegration
	err := db.scoped(customerID).Where("name = ?", name).First(&item).Error
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// listPagerDuty mirrors the former Store.ListPagerDutyIntegrations.
func listPagerDuty(db *Store, customerID uint) ([]PagerDutyIntegration, error) {
	var items []PagerDutyIntegration
	err := db.scoped(customerID).Order("name").Find(&items).Error
	return items, err
}

// deleteAllPagerDuty mirrors the former Store.DeleteAllPagerDutyIntegrations.
func deleteAllPagerDuty(db *Store, customerID uint) error {
	return db.scoped(customerID).Delete(&PagerDutyIntegration{}).Error
}
