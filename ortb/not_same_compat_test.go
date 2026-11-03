package ortb

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

func assertNotSameReference(t *testing.T, expected, actual interface{}, msgAndArgs ...interface{}) bool {
	t.Helper()
	expectedValue := reflect.ValueOf(expected)
	actualValue := reflect.ValueOf(actual)
	if !expectedValue.IsValid() || !actualValue.IsValid() || expectedValue.Kind() != actualValue.Kind() {
		return true
	}

	switch expectedValue.Kind() {
	case reflect.Ptr:
		return assert.NotSame(t, expected, actual, msgAndArgs...)
	case reflect.Map:
		return assert.NotEqual(t, expectedValue.UnsafePointer(), actualValue.UnsafePointer(), msgAndArgs...)
	case reflect.Slice:
		if expectedValue.Len() == 0 || actualValue.Len() == 0 {
			return true
		}
		return assert.NotEqual(t, expectedValue.UnsafePointer(), actualValue.UnsafePointer(), msgAndArgs...)
	default:
		return true
	}
}
