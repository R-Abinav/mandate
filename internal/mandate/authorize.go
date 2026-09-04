// Package mandate — authorize.go
//
// This file previously contained AuthorizeMandate, which attempted to perform
// S2S card authorization via client.Payment.CreateRecurringPayment.
// That endpoint (/v1/payments/create/recurring) returned 404 on this account.
//
// The active demo path now uses CreateRegistrationLink in register.go, which
// generates a Razorpay-hosted auth page — Razorpay's intended product for
// card-CoFT mandate registration without PCI-DSS scope on the merchant side.
//
// See docs/adr/ADR-0003-registration-link-auth.md for the full decision record.
package mandate
