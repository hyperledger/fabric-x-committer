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

// registerDefaults registers every field's `default:"..."` tag value as a viper default, the lowest
// precedence.
//
// The walk is driven by the struct TYPE (not a value) on purpose: defaults must reach nested pointer
// fields that are absent from the YAML, whose pointers are still nil before decoding. Validation is
// the mirror image - it needs the decoded VALUES and skips nil optional branches - so it is a
// separate post-decode pass and cannot share this walk.
//
// It stops at a list. Viper holds a list as one value, so a default registered at a path inside it
// never reaches the listed items and, when the list is absent, conjures a phantom item. A default a
// list item needs must be resolved in code.
func registerDefaults(v *viper.Viper, objType reflect.Type, keyParts ...string) {
	for objType.Kind() == reflect.Pointer {
		objType = objType.Elem()
	}
	if objType.Kind() != reflect.Struct {
		return
	}
	for field := range objType.Fields() {
		fieldKeyParts, ok := getFieldKeyParts(field, keyParts...)
		if !ok {
			continue
		}
		if defaultValue, ok := field.Tag.Lookup("default"); ok {
			registerDefault(v, strings.Join(fieldKeyParts, "."), defaultValue)
		}
		registerDefaults(v, field.Type, fieldKeyParts...)
	}
}

// registerDefault registers a field's `default:"..."` tag value in viper.
//
// A plain value is registered at the field's own path and converted to the field's type by the
// decode hooks (e.g. "9001" -> int, "5s" -> time.Duration, "localhost:5433" -> connection.Endpoint).
//
// A value written as one or more space-separated key=value pairs is treated as a struct assignment:
// each pair is registered under a nested sub-key of the field's path. This lets a nested struct's
// defaults be declared on the embedding field and scoped to that use, rather than on a shared type
// (e.g. `default:"max-elapsed-time=10m"` on a *retry.Profile field sets only <path>.max-elapsed-time,
// leaving other users of retry.Profile untouched).
func registerDefault(v *viper.Viper, path, defaultValue string) {
	if !strings.Contains(defaultValue, "=") {
		v.SetDefault(path, defaultValue)
		return
	}
	for pair := range strings.FieldsSeq(defaultValue) {
		if key, val, ok := strings.Cut(pair, "="); ok {
			v.SetDefault(path+"."+key, val)
		}
	}
}

// applyEnvOverrides applies every matching environment-variable override, the highest precedence.
// Like registerDefaults it walks the struct TYPE, so an override reaches a section absent from the
// YAML. Unlike registerDefaults it walks into lists: that is how a single list item is set from the
// environment.
func applyEnvOverrides(v *viper.Viper, objType reflect.Type, keyParts ...string) {
	applyEnvOverride(v, keyParts)

	for objType.Kind() == reflect.Pointer {
		objType = objType.Elem()
	}
	switch objType.Kind() {
	case reflect.Array, reflect.Slice:
		applyEnvOverrides(v, objType.Elem(), keyParts...)
	case reflect.Struct:
		for field := range objType.Fields() {
			if fieldKeyParts, ok := getFieldKeyParts(field, keyParts...); ok {
				applyEnvOverrides(v, field.Type, fieldKeyParts...)
			}
		}
	default:
	}
}

// applyEnvOverride sets the value for the current path from a matching environment variable
// (highest precedence). It also fires for intermediate struct paths so a nested pointer struct
// can be overridden wholesale.
func applyEnvOverride(v *viper.Viper, keyParts []string) {
	fieldKey := strings.Join(keyParts, ".")
	if fieldKey == "" {
		return
	}
	envKey := strings.ToUpper(envStringReplacer.Replace(v.GetEnvPrefix() + "_" + fieldKey))
	if envValue, ok := os.LookupEnv(envKey); ok && envValue != "" {
		logger.Debugf("Overriding config key '%s' from env var '%s'", fieldKey, envKey)
		v.Set(fieldKey, envValue)
	}
}

// getFieldKeyParts appends a field's dotted mapstructure key to keyParts, honoring `,squash`
// (embedded fields share the parent path) and skipping fields without a usable mapstructure tag.
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
