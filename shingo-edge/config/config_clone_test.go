package config

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// fillConfig sets every exported field reachable from v to a non-zero value
// derived from seed: strings, numbers, bools, durations, times, every slice
// (two elements) and every pointer. Two different seeds give two configs that
// differ in every leaf, which is how the alias check below sees a shared slice
// or pointer.
func fillConfig(t *testing.T, v reflect.Value, seed int, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(int64(1_700_000_000+seed), 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			fillConfig(t, v.Field(i), seed, path+"."+f.Name)
		}
	case reflect.String:
		v.SetString(fmt.Sprintf("%s#%d", path, seed))
	case reflect.Bool:
		v.SetBool(seed%2 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(seed))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(seed) + 0.5)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < 2; i++ {
			fillConfig(t, s.Index(i), seed+i, fmt.Sprintf("%s[%d]", path, i))
		}
		v.Set(s)
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		fillConfig(t, p.Elem(), seed, path+"*")
		v.Set(p)
	default:
		t.Fatalf("fillConfig: %s has kind %s, which the test does not fill; teach it, so the clone check covers it", path, v.Kind())
	}
}

func filled(t *testing.T, seed int) *Config {
	t.Helper()
	c := &Config{}
	fillConfig(t, reflect.ValueOf(c).Elem(), seed, "Config")
	return c
}

// assertNoZeroLeaf fails on any exported leaf left at its zero value, so the
// filler really did reach every field (a zero field would pass a DeepEqual
// even if Clone dropped it).
func assertNoZeroLeaf(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			if v.Interface().(time.Time).IsZero() {
				t.Errorf("%s is zero", path)
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				assertNoZeroLeaf(t, v.Field(i), path+"."+f.Name)
			}
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
		for i := 0; i < v.Len(); i++ {
			assertNoZeroLeaf(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Ptr:
		if v.IsNil() {
			t.Errorf("%s is nil", path)
			return
		}
		assertNoZeroLeaf(t, v.Elem(), path+"*")
	case reflect.Bool:
		// seed 1 sets every bool true.
		if !v.Bool() {
			t.Errorf("%s is false", path)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero", path)
		}
	}
}

// TestConfig_CloneAndAdoptCoverEveryField: the one save path copies the live
// config (Clone), applies the posted sections to the copy, writes it, and
// swaps it in field by field (Adopt). A field either step forgets would be
// silently reset on every page save. Every field is set non-zero here by
// reflection, so a field added to Config later fails this test until it is
// added to assignFrom.
func TestConfig_CloneAndAdoptCoverEveryField(t *testing.T) {
	orig := filled(t, 1)
	assertNoZeroLeaf(t, reflect.ValueOf(orig).Elem(), "Config")

	clone := orig.Clone()
	if !reflect.DeepEqual(clone, orig) {
		t.Fatalf("Clone dropped or changed a field:\n got %+v\nwant %+v", clone, orig)
	}
	// GroupID is yaml:"-": a yaml round trip would lose it; the Go clone keeps it.
	if clone.Messaging.Kafka.GroupID != orig.Messaging.Kafka.GroupID {
		t.Errorf("Clone lost KafkaConfig.GroupID")
	}

	live := &Config{}
	live.Adopt(clone)
	if !reflect.DeepEqual(live, orig) {
		t.Fatalf("Adopt dropped or changed a field:\n got %+v\nwant %+v", live, orig)
	}
}

// TestConfig_CloneSharesNothing: writing every leaf of the clone (slices and
// pointers included) leaves the original as it was. A shared backing array or
// pointer would let a page's draft reach the live config before validation.
func TestConfig_CloneSharesNothing(t *testing.T) {
	orig := filled(t, 1)
	clone := orig.Clone()
	// Overwrite every leaf IN PLACE, through the clone's own slices and
	// pointers, so an alias shows up in orig.
	overwriteInPlace(t, reflect.ValueOf(clone).Elem(), 7, "Config")
	if want := filled(t, 1); !reflect.DeepEqual(orig, want) {
		t.Fatalf("writing the clone changed the original:\n got %+v\nwant %+v", orig, want)
	}
}

// overwriteInPlace writes new leaf values without replacing any slice header
// or pointer, so the writes land in whatever memory the clone points at.
func overwriteInPlace(t *testing.T, v reflect.Value, seed int, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(int64(1_800_000_000+seed), 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				overwriteInPlace(t, v.Field(i), seed, path+"."+f.Name)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			overwriteInPlace(t, v.Index(i), seed, fmt.Sprintf("%s[%d]", path, i))
		}
	case reflect.Ptr:
		if !v.IsNil() {
			overwriteInPlace(t, v.Elem(), seed, path+"*")
		}
	default:
		fillConfig(t, v, seed, path)
	}
}

// TestConfig_SaveMutexIsSeparate: LockSave does not take the field lock, so a
// holder of the save mutex can still read and Adopt.
func TestConfig_SaveMutexIsSeparate(t *testing.T) {
	c := Defaults()
	c.LockSave()
	defer c.UnlockSave()
	c.RLock()
	_ = c.Timezone
	c.RUnlock()
	c.Adopt(c.Clone())
}
