package helpers

import (
	"fmt"
	"strconv"
	"strings"
)

func ResolveUnionIntOrStringValue(input any) int {
	switch value := input.(type) {
	case int:
		return value
	case uint:
		return int(value)
	case string:
		intValue, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			return int(intValue)
		}

		result, err := convertSizeUnitToByte(value)
		if err != nil {
			panic(err)
		}

		return result
	}

	return 0
}

func convertSizeUnitToByte(str string) (int, error) {
	if len(str) < 2 {
		return 0, fmt.Errorf("invalid input: %s", str)
	}

	// Extract the numeric part of the input
	sizeStr := str[:len(str)-2]
	sizeStr = strings.TrimSpace(sizeStr)
	sizeStr = strings.ReplaceAll(sizeStr, ",", ".")

	size, err := strconv.ParseFloat(sizeStr, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot extract numeric part for the input %s, err = %w", str, err)
	}

	// Determine the unit (B, KB, MB, GB)
	unit := str[len(str)-2:]
	switch strings.ToUpper(unit) {
	case "B":
		return int(size), nil
	case "KB":
		return int(size * 1024), nil
	case "MB":
		return int(size * 1024 * 1024), nil
	case "GB":
		return int(size * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unsupported unit: %s, you can specify one of B, KB, MB and GB", unit)
	}
}
