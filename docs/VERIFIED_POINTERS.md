## Regulatory Context: RBI E-mandate Framework

**Source:** RBI Circular RBI/DPSS/2026-27/396, dated April 21, 2026, "Digital Payments – E-mandate Framework, 2026."
Primary source: https://rbi.org.in/Scripts/NotificationUser.aspx?Id=13374
(PDF: https://rbidocs.rbi.org.in/rdocs/notification/PDFs/396MDD002E435ECA145509929FC3ACBCFD0E9.PDF).
Confirmed directly against RBI's own site. Consolidates eight prior circulars issued 2019–2024.

- **First-transaction AFA (sourced):** The first transaction under any e-mandate always requires full Additional Factor Authentication (AFA/2FA) — no exception, even when combined with registration.

- **Recurring-debit AFA thresholds (sourced):** Recurring debits up to ₹15,000 per transaction may process without AFA. Insurance premiums, mutual fund/SIP subscriptions, and credit card bill payments specifically get a higher ₹1,00,000 no-AFA ceiling. Transactions above the applicable threshold require AFA every time.

- **Notification obligations (sourced):** Mandatory 24-hour pre-debit notification (merchant name, amount, date, mandate reference, opt-out option) and mandatory post-debit confirmation (including grievance redressal details) are required for every debit under the framework.

- **"Transaction Limits and Velocity Check" section (sourced with caveat):** The circular's own section header is "Transaction Limits and Velocity Check." Based on five independent legal-analysis sources describing this section (IndiaLaw LLP, KSandK, Sagus Legal, SCC Online, Global Law Experts), its substance is the per-transaction AFA ceiling above — none describe an actual cumulative or per-agent rate-limiting mechanism under that heading. **Caveat, stated explicitly, not hidden:** RBI's own PDF could not be read verbatim for this specific clause due to a CAPTCHA wall on rbidocs.rbi.org.in; this conclusion rests on convergent secondary-source agreement, not direct primary-text confirmation. Recommend one more verification attempt before final submission if time allows.

- **The gap this project fills (derived from above):** RBI's threshold is uniform, network-wide, and per-transaction — identical for every mandate, every merchant, every agent. It provides no mechanism for a merchant to impose a tighter, agent-specific, cumulative or category-based spend policy on top of a standing mandate they've granted. That is the actual gap — not "RBI has no rules," but "RBI's rules don't reach this layer."
