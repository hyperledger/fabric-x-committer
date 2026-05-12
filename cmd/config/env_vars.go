/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

// setEnvVars recursively walks through struct fields and manually sets environment variables in viper.
// This allows environment variables to override nested pointer struct fields.
func setEnvVars(v *viper.Viper, objType reflect.Type, keyParts ...string) {
	if fieldKey := strings.Join(keyParts, "."); len(fieldKey) > 0 {
		envKey := strings.ToUpper(envStringReplacer.Replace(v.GetEnvPrefix() + "_" + fieldKey))
		if envValue, ok := os.LookupEnv(envKey); ok && envValue != "" {
			logger.Debugf("Overridding config key '%s' from env var '%s'", fieldKey, envKey)
			v.Set(fieldKey, envValue)
		}
	}

	// Dereference pointer types.
	for objType.Kind() == reflect.Ptr {
		objType = objType.Elem()
	}
	switch objType.Kind() {
	case reflect.Array, reflect.Slice:
		setEnvVars(v, objType.Elem(), keyParts...)
	case reflect.Struct:
		for i := range objType.NumField() {
			field := objType.Field(i)
			if fieldKeyParts, ok := getFieldKeyParts(field, keyParts...); ok {
				setEnvVars(v, field.Type, fieldKeyParts...)
			}
		}
	default:
	}
}

func getFieldKeyParts(field reflect.StructField, keyParts ...string) ([]string, bool) {
	mapTag, ok := field.Tag.Lookup("mapstructure")
	if !field.IsExported() || !ok || mapTag == "" || mapTag == "-" {
		return nil, false
	}
	attributes := strings.Split(mapTag, ",")
	for i, a := range attributes {
		attributes[i] = strings.Trim(a, " ")
	}

	if slices.Contains(attributes[1:], "squash") {
		return keyParts, true
	}

	return append(keyParts, attributes[0]), true
}
