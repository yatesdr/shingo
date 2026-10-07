package config

import (
	"reflect"
	"testing"
	"time"
)

// fillNonZero sets every exported field reachable from v to a non-zero value,
// recursing into structs. Unexported fields (the mutex) are left alone. seed
// varies the values so two fills differ.
func fillNonZero(t *testing.T, v reflect.Value, seed int, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5+seed, 0, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			fillNonZero(t, v.Field(i), seed+i, path+"."+f.Name)
		}
	case reflect.String:
		v.SetString(path + "#" + string(rune('a'+seed%26)))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(seed + 7))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(seed + 7))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(seed) + 0.5)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < 2; i++ {
			fillNonZero(t, s.Index(i), seed+i, path+"[]")
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(t, k, seed, path+"{k}")
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, e, seed+1, path+"{v}")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		fillNonZero(t, p.Elem(), seed, path+"*")
		v.Set(p)
	default:
		t.Fatalf("%s: kind %v not handled by fillNonZero; extend it", path, v.Kind())
	}
}

// assertNoZero fails on any exported leaf that is still zero, so the fill
// itself is proven complete before it is used to judge Clone/ReplaceFrom.
func assertNoZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	if v.Kind() == reflect.Struct && v.Type() != reflect.TypeOf(time.Time{}) {
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.IsExported() {
				assertNoZero(t, v.Field(i), path+"."+f.Name)
			}
		}
		return
	}
	if v.IsZero() {
		t.Errorf("%s is zero after fill", path)
	}
}

// TestConfigCloneAndReplaceCoverEveryField is the "one save path" guard: the
// config page saves by cloning the live config, editing the clone and swapping
// it back field by field. A top-level field added to Config and not to Clone
// or ReplaceFrom would be silently reset by every save; this fails instead.
func TestConfigCloneAndReplaceCoverEveryField(t *testing.T) {
	src := &Config{}
	fillNonZero(t, reflect.ValueOf(src).Elem(), 1, "Config")
	assertNoZero(t, reflect.ValueOf(src).Elem(), "Config")

	clone := src.Clone()
	if !reflect.DeepEqual(clone, src) {
		t.Fatalf("Clone dropped or changed a field:\n got %+v\nwant %+v", clone, src)
	}
	// The clone's slices are its own.
	clone.Messaging.Kafka.Brokers[0] = "changed"
	clone.Notifications.Recipients[0] = "changed"
	clone.Logging.StderrSubsystems[0] = "changed"
	if src.Messaging.Kafka.Brokers[0] == "changed" || src.Notifications.Recipients[0] == "changed" ||
		src.Logging.StderrSubsystems[0] == "changed" {
		t.Fatal("Clone shares a slice's backing array with the original")
	}

	draft := &Config{}
	fillNonZero(t, reflect.ValueOf(draft).Elem(), 3, "Config")
	live := &Config{}
	live.ReplaceFrom(draft)
	if !reflect.DeepEqual(live, draft) {
		t.Fatalf("ReplaceFrom dropped or changed a field:\n got %+v\nwant %+v", live, draft)
	}
	// ReplaceFrom leaves the live mutex usable.
	if !live.TryLock() {
		t.Fatal("live config's mutex is held after ReplaceFrom")
	}
	live.Unlock()
}

// The swap writes into the same struct: a pointer a subsystem took into the
// live config before the save sees the saved value after it.
func TestConfigReplaceFromKeepsPointersIntoLive(t *testing.T) {
	live := Defaults()
	held := &live.Messaging
	draft := live.Clone()
	draft.Messaging.OrdersTopic = "after.save"
	live.ReplaceFrom(draft)
	if held.OrdersTopic != "after.save" {
		t.Fatalf("pointer into live config sees %q, want after.save", held.OrdersTopic)
	}
}
