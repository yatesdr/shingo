package notify

import (
	"fmt"
	"strings"
	"time"
)

func FaultAlert(orderID int64, edgeUUID, stationID, reason, robotID string) string {
	var b strings.Builder
	b.WriteString("SHINGO FAULT ALERT\n")
	b.WriteString("==================\n\n")
	b.WriteString(fmt.Sprintf("Order ID:     %d\n", orderID))
	if edgeUUID != "" {
		b.WriteString(fmt.Sprintf("Edge UUID:    %s\n", edgeUUID))
	}
	if stationID != "" {
		b.WriteString(fmt.Sprintf("Station:      %s\n", stationID))
	}
	if robotID != "" {
		b.WriteString(fmt.Sprintf("Robot ID:     %s\n", robotID))
	}
	b.WriteString(fmt.Sprintf("Reason:       %s\n", reason))
	b.WriteString(fmt.Sprintf("Time:         %s\n", time.Now().Format(time.RFC1123)))
	b.WriteString("\n")
	b.WriteString("The order has entered a faulted grace period.\n")
	b.WriteString("If the fleet does not recover within the configured grace window, the order will automatically fail.\n")
	b.WriteString("\n\n\n")
	return b.String()
}

func FailAlert(orderID int64, edgeUUID, stationID, errorCode, detail, robotID string) string {
	var b strings.Builder
	b.WriteString("SHINGO ORDER FAILURE ALERT\n")
	b.WriteString("=========================\n\n")
	b.WriteString(fmt.Sprintf("Order ID:     %d\n", orderID))
	if edgeUUID != "" {
		b.WriteString(fmt.Sprintf("Edge UUID:    %s\n", edgeUUID))
	}
	if stationID != "" {
		b.WriteString(fmt.Sprintf("Station:      %s\n", stationID))
	}
	if robotID != "" {
		b.WriteString(fmt.Sprintf("Robot ID:     %s\n", robotID))
	}
	if errorCode != "" {
		b.WriteString(fmt.Sprintf("Error Code:   %s\n", errorCode))
	}
	if detail != "" {
		b.WriteString(fmt.Sprintf("Detail:       %s\n", detail))
	}
	b.WriteString(fmt.Sprintf("Time:         %s\n", time.Now().Format(time.RFC1123)))
	b.WriteString("\n")
	b.WriteString("An order has reached a terminal failed state.\n")
	b.WriteString("\n\n\n")
	return b.String()
}

func GraceExpiredAlert(orderID int64, vendorOrderID, robotID string) string {
	var b strings.Builder
	b.WriteString("SHINGO GRACE EXPIRED ALERT\n")
	b.WriteString("==========================\n\n")
	b.WriteString(fmt.Sprintf("Order ID:        %d\n", orderID))
	if vendorOrderID != "" {
		b.WriteString(fmt.Sprintf("Vendor Order ID: %s\n", vendorOrderID))
	}
	if robotID != "" {
		b.WriteString(fmt.Sprintf("Robot ID:         %s\n", robotID))
	}
	b.WriteString(fmt.Sprintf("Time:            %s\n", time.Now().Format(time.RFC1123)))
	b.WriteString("\n")
	b.WriteString("A faulted order's grace period has expired without fleet recovery.\n")
	b.WriteString("The order has been automatically failed and cancelled at the vendor.\n")
	return b.String()
}

func FaultSubject(robotID string) string {
	if robotID != "" {
		return fmt.Sprintf("Shingo Fault Alert - Robot %s", robotID)
	}
	return "Shingo Fault Alert"
}

func FailSubject(robotID string) string {
	if robotID != "" {
		return fmt.Sprintf("Shingo Order Failure - Robot %s", robotID)
	}
	return "Shingo Order Failure"
}

func GraceExpiredSubject() string {
	return "Shingo Grace Period Expired"
}

func FaultClearedSubject(robotID string) string {
	if robotID != "" {
		return fmt.Sprintf("Shingo Fault Cleared - Robot %s", robotID)
	}
	return "Shingo Fault Cleared"
}

func FaultClearedAlert(orderID int64, edgeUUID, stationID, robotID, timeFaulted string) string {
	var b strings.Builder
	b.WriteString("SHINGO FAULT CLEARED\n")
	b.WriteString("====================\n\n")
	b.WriteString(fmt.Sprintf("Order ID:     %d\n", orderID))
	if edgeUUID != "" {
		b.WriteString(fmt.Sprintf("Edge UUID:    %s\n", edgeUUID))
	}
	if stationID != "" {
		b.WriteString(fmt.Sprintf("Station:      %s\n", stationID))
	}
	if robotID != "" {
		b.WriteString(fmt.Sprintf("Robot ID:     %s\n", robotID))
	}
	if timeFaulted != "" {
		b.WriteString(fmt.Sprintf("Time Faulted: %s\n", timeFaulted))
	}
	b.WriteString(fmt.Sprintf("Time:         %s\n", time.Now().Format(time.RFC1123)))
	b.WriteString("\n")
	b.WriteString("The faulted order has recovered and resumed normal operation.\n")
	b.WriteString("\n\n\n")
	return b.String()
}
