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

func FaultSubject() string {
	return fmt.Sprintf("Shingo Fault Alert - %s", time.Now().Format("2006-01-02 15:04:05"))
}

func FailSubject() string {
	return fmt.Sprintf("Shingo Order Failure - %s", time.Now().Format("2006-01-02 15:04:05"))
}

func GraceExpiredSubject() string {
	return fmt.Sprintf("Shingo Grace Period Expired - %s", time.Now().Format("2006-01-02 15:04:05"))
}
