package db

import (
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_2_0_AddKeyManagementFields adds priority and is_manually_disabled fields to api_keys table
func V1_2_0_AddKeyManagementFields(db *gorm.DB) error {
	logrus.Info("Running migration: v1.2.0 - Add key management fields")

	// Check if priority column exists
	var priorityExists bool
	if db.Dialector.Name() == "mysql" {
		var count int64
		db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = 'api_keys'
			AND COLUMN_NAME = 'priority'
		`).Count(&count)
		priorityExists = count > 0
	} else if db.Dialector.Name() == "postgres" {
		var count int64
		db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_name = 'api_keys'
			AND column_name = 'priority'
		`).Count(&count)
		priorityExists = count > 0
	} else {
		// SQLite
		var count int64
		db.Raw("SELECT COUNT(*) FROM pragma_table_info('api_keys') WHERE name='priority'").Count(&count)
		priorityExists = count > 0
	}

	if !priorityExists {
		logrus.Info("Adding priority column to api_keys table")
		if err := db.Exec("ALTER TABLE api_keys ADD COLUMN priority INTEGER NOT NULL DEFAULT 0").Error; err != nil {
			logrus.Errorf("Failed to add priority column: %v", err)
			return err
		}
		logrus.Info("Successfully added priority column")
	} else {
		logrus.Info("priority column already exists, skipping")
	}

	// Check if is_manually_disabled column exists
	var manuallyDisabledExists bool
	if db.Dialector.Name() == "mysql" {
		var count int64
		db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			AND TABLE_NAME = 'api_keys'
			AND COLUMN_NAME = 'is_manually_disabled'
		`).Count(&count)
		manuallyDisabledExists = count > 0
	} else if db.Dialector.Name() == "postgres" {
		var count int64
		db.Raw(`
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_name = 'api_keys'
			AND column_name = 'is_manually_disabled'
		`).Count(&count)
		manuallyDisabledExists = count > 0
	} else {
		// SQLite
		var count int64
		db.Raw("SELECT COUNT(*) FROM pragma_table_info('api_keys') WHERE name='is_manually_disabled'").Count(&count)
		manuallyDisabledExists = count > 0
	}

	if !manuallyDisabledExists {
		logrus.Info("Adding is_manually_disabled column to api_keys table")
		if err := db.Exec("ALTER TABLE api_keys ADD COLUMN is_manually_disabled BOOLEAN NOT NULL DEFAULT FALSE").Error; err != nil {
			logrus.Errorf("Failed to add is_manually_disabled column: %v", err)
			return err
		}
		logrus.Info("Successfully added is_manually_disabled column")
	} else {
		logrus.Info("is_manually_disabled column already exists, skipping")
	}

	logrus.Info("Migration v1.2.0 completed successfully")
	return nil
}
