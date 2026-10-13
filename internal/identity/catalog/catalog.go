// Code generated from docs/rbac.md by the Stage 5 build and verified against it by catalog_test.go. DO NOT EDIT BY HAND:
// change docs/rbac.md first, then this file, then the test will tell you if they drift.

// Package catalog holds the permission keys and starter role templates (brief section 4).
// Cells marked limited in docs/rbac.md (scope notes) are granted here; the service layer narrows them.
package catalog

type Permission struct {
	Key         string
	Description string
	Protected   bool // only protected roles may hold it
}

type Role struct {
	Code        string
	Name        string
	Description string
	Protected   bool
	Permissions []string
}

var Permissions = []Permission{
	{Key: "sales.view", Description: "View sales and receipts"},
	{Key: "sales.create", Description: "Create and finalise sales, record payments"},
	{Key: "sales.discount", Description: "Apply discounts"},
	{Key: "sales.void", Description: "Void sales"},
	{Key: "sales.refund", Description: "Refund sales"},
	{Key: "sales.reprint", Description: "Reprint receipts"},
	{Key: "orders.manage", Description: "Create and edit orders, send to kitchen"},
	{Key: "kitchen.view", Description: "View kitchen tickets"},
	{Key: "kitchen.update", Description: "Update kitchen ticket status"},
	{Key: "catalogue.manage", Description: "Manage menu, prices and tables"},
	{Key: "shifts.manage", Description: "Open shifts and record cash movements"},
	{Key: "shifts.close", Description: "Close shifts"},
	{Key: "payments.reconcile", Description: "Confirm manual payments and reconcile outage receipts"},
	{Key: "inventory.view", Description: "View stock"},
	{Key: "inventory.adjust", Description: "Stock adjustments, counts and item master"},
	{Key: "inventory.transfer", Description: "Transfer stock between locations"},
	{Key: "inventory.wastage", Description: "Record wastage, breakage and complimentary stock"},
	{Key: "inventory.override_negative", Description: "Override a negative-stock warning"},
	{Key: "purchasing.manage", Description: "Suppliers, purchases and receiving"},
	{Key: "rooms.view", Description: "View in-house room list"},
	{Key: "rooms.manage", Description: "Manage rooms, guests, reservations and room status"},
	{Key: "folios.view", Description: "View guest folios"},
	{Key: "folios.post", Description: "Post authorised room charges"},
	{Key: "folios.manage", Description: "Settle, adjust, write off and close folios"},
	{Key: "reports.view", Description: "View reports"},
	{Key: "reports.export", Description: "Export reports to CSV"},
	{Key: "audit.view", Description: "View audit log"},
	{Key: "users.create", Description: "Create users"},
	{Key: "users.update", Description: "Edit users and assign roles"},
	{Key: "users.deactivate", Description: "Deactivate users"},
	{Key: "users.delete", Description: "Delete users with no history"},
	{Key: "roles.manage", Description: "Create and edit roles"},
	{Key: "permissions.assign", Description: "Assign permissions to roles"},
	{Key: "system.admin", Description: "System administration (protected)", Protected: true},
}

var Roles = []Role{
	{Code: "super_admin", Name: "Super Admin", Description: "Owner and recovery role: users, roles, permissions, settings and audit. Non-operational by design.", Protected: true, Permissions: []string{
		"audit.view",
		"users.create",
		"users.update",
		"users.deactivate",
		"users.delete",
		"roles.manage",
		"permissions.assign",
		"system.admin",
	}},
	{Code: "business_manager", Name: "Business Manager", Description: "Full operational access without system administration.", Permissions: []string{
		"sales.view",
		"sales.create",
		"sales.discount",
		"sales.void",
		"sales.refund",
		"sales.reprint",
		"orders.manage",
		"kitchen.view",
		"kitchen.update",
		"catalogue.manage",
		"shifts.manage",
		"shifts.close",
		"payments.reconcile",
		"inventory.view",
		"inventory.adjust",
		"inventory.transfer",
		"inventory.wastage",
		"inventory.override_negative",
		"purchasing.manage",
		"rooms.view",
		"rooms.manage",
		"folios.view",
		"folios.post",
		"folios.manage",
		"reports.view",
		"reports.export",
		"audit.view",
		"users.create",
		"users.update",
		"users.deactivate",
	}},
	{Code: "cashier", Name: "Cashier", Description: "Takes payments, manages own shift, reprints receipts.", Permissions: []string{
		"sales.view",
		"sales.create",
		"sales.reprint",
		"orders.manage",
		"kitchen.view",
		"shifts.manage",
		"shifts.close",
		"rooms.view",
		"folios.post",
		"reports.view",
	}},
	{Code: "waiter", Name: "Waiter", Description: "Creates and manages orders, sends to kitchen, posts authorised room charges.", Permissions: []string{
		"sales.view",
		"orders.manage",
		"kitchen.view",
		"kitchen.update",
		"rooms.view",
		"folios.post",
	}},
	{Code: "bartender", Name: "Bartender", Description: "Bar orders and sales, authorised bar stock operations.", Permissions: []string{
		"sales.view",
		"sales.create",
		"sales.reprint",
		"orders.manage",
		"shifts.manage",
		"shifts.close",
		"inventory.view",
		"inventory.transfer",
		"inventory.wastage",
		"rooms.view",
		"folios.post",
	}},
	{Code: "receptionist", Name: "Receptionist", Description: "Reservations, check-in and check-out, room status, folio settlement.", Permissions: []string{
		"rooms.view",
		"rooms.manage",
		"folios.view",
		"folios.post",
		"folios.manage",
	}},
	{Code: "storekeeper", Name: "Storekeeper", Description: "Receiving, transfers, counts and adjustments.", Permissions: []string{
		"inventory.view",
		"inventory.adjust",
		"inventory.transfer",
		"inventory.wastage",
		"purchasing.manage",
	}},
	{Code: "kitchen_staff", Name: "Kitchen staff", Description: "Views and updates kitchen tickets, records kitchen wastage.", Permissions: []string{
		"kitchen.view",
		"kitchen.update",
		"inventory.wastage",
	}},
	{Code: "accountant_auditor", Name: "Accountant / Auditor", Description: "Read-only reports, exports and audit.", Permissions: []string{
		"sales.view",
		"inventory.view",
		"rooms.view",
		"folios.view",
		"reports.view",
		"reports.export",
		"audit.view",
	}},
}
