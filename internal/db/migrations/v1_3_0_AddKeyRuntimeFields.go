package db

import (
	"gpt-load/internal/models"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// V1_3_0_AddKeyRuntimeFields adds runtime status tracking fields to api_keys table.
func V1_3_0_AddKeyRuntimeFields(db *gorm.DB) error {
	logrus.Info("Running migration: v1.3.0 - Add key runtime fields")

	columns := []string{
		"CooldownUntil",
		"LastErrorCode",
		"LastErrorMessage",
		"LastStatusAction",
	}

	for _, column := range columns {
		if db.Migrator().HasColumn(&models.APIKey{}, column) {
			logrus.Infof("%s column already exists, skipping", column)
			continue
		}

		logrus.Infof("Adding %s column to api_keys table", column)
		if err := db.Migrator().AddColumn(&models.APIKey{}, column); err != nil {
			logrus.Errorf("Failed to add %s column: %v", column, err)
			return err
		}
	}

	logrus.Info("Migration v1.3.0 completed successfully")
	return nil
}
