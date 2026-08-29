package registry_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/gaarutyunov/goga/registry"
)

// koanfDecode is a deliberately tiny stand-in for what goga/config will inject.
// It honours `koanf:"..."` tags so the tests can prove that a tag whose value
// differs from its field name is what actually selects the key, and it rejects
// unknown keys the way the [registry.Decode] contract asks a real decoder to.
//
// It is hand-rolled on top of reflect on purpose: the registry package must not
// grow a koanf dependency, not even in its tests.
func koanfDecode(raw registry.Settings, dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("dst must point to a struct, got %s", v.Type())
	}

	byKey := make(map[string]int, v.NumField())
	for i := range v.NumField() {
		f := v.Type().Field(i)
		key := f.Tag.Get("koanf")
		if key == "" {
			key = strings.ToLower(f.Name)
		}
		byKey[key] = i
	}

	var unknown []string
	for key, val := range raw {
		i, ok := byKey[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		field := v.Field(i)
		if !field.CanSet() {
			return fmt.Errorf("field for key %q is not settable", key)
		}
		rv := reflect.ValueOf(val)
		switch {
		case !rv.IsValid():
			field.SetZero()
		case rv.Type().AssignableTo(field.Type()):
			field.Set(rv)
		case rv.Type().ConvertibleTo(field.Type()):
			field.Set(rv.Convert(field.Type()))
		default:
			return fmt.Errorf("cannot assign %s to key %q of type %s", rv.Type(), key, field.Type())
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("unknown keys: %s", strings.Join(unknown, ", "))
	}
	return nil
}
