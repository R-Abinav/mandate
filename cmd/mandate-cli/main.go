// Mandate-cli is the natural-language policy setup CLI: a two-step
// propose/confirm flow. propose parses free text into structured policy
// numbers and stages them; confirm is the only command, anywhere in this
// codebase, that writes to the real policies table.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/R-Abinav/mandate/internal/config"
	"github.com/R-Abinav/mandate/internal/logging"
	"github.com/R-Abinav/mandate/internal/policy"
	"github.com/R-Abinav/mandate/internal/store"
)

func main() {
	logger := logging.New(config.Load().LogLevel)
	if err := run(os.Args[1:]); err != nil {
		logger.Error("mandate-cli: fatal", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "propose":
		return runPropose(args[1:])
	case "confirm":
		return runConfirm(args[1:])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New(
		"usage:\n" +
			"  mandate-cli propose <policy_id> <agent_id> \"<free text>\"\n" +
			"  mandate-cli confirm <proposal_id>",
	)
}

// policyWriter is the minimal capability confirm needs to activate a
// policy. Defined narrowly, right here, rather than importing the full
// store.PolicyStore interface — propose's code path below never even sees
// this type, so it is structurally impossible for propose to hold a value
// satisfying it, regardless of what the LLM returns.
type policyWriter interface {
	SavePolicy(ctx context.Context, p policy.Policy) error
}

func runPropose(args []string) error {
	if len(args) != 3 {
		return usageError()
	}
	policyID, agentID, text := args[0], args[1], args[2]
	if strings.TrimSpace(agentID) == "" {
		return errors.New("mandate-cli: agent_id must not be empty")
	}

	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return errors.New("mandate-cli: DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("mandate-cli: failed to open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("mandate-cli: database unreachable: %w", err)
	}

	proposalStore := store.NewPostgresProposalStore(db)

	// The provider selected by LLM_PROVIDER (anthropic or google) is the
	// only one whose key is checked — swapping providers must not require
	// both API keys to be present.
	llm, err := policy.NewLLMClient(context.Background(), policy.LLMConfig{
		Provider:        cfg.LLMProvider,
		AnthropicAPIKey: cfg.AnthropicAPIKey,
		AnthropicModel:  cfg.AnthropicModel,
		GeminiAPIKey:    cfg.GeminiAPIKey,
		GeminiModel:     cfg.GeminiModel,
	})
	if err != nil {
		return fmt.Errorf("mandate-cli: %w", err)
	}

	// Note exactly what proposeCommand receives: a Store.ProposalStore
	// (the ephemeral staging table) and nothing capable of writing a real
	// policy. There is no policyWriter in this call.
	return proposeCommand(
		context.Background(),
		os.Stdout,
		llm,
		proposalStore,
		policyID,
		agentID,
		text,
	)
}

func runConfirm(args []string) error {
	if len(args) != 1 {
		return usageError()
	}
	proposalID := args[0]

	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		return errors.New("mandate-cli: DATABASE_URL is required")
	}

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("mandate-cli: failed to open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("mandate-cli: database unreachable: %w", err)
	}

	proposalStore := store.NewPostgresProposalStore(db)
	policyStore := store.NewPostgresPolicyStore(db)

	return confirmCommand(context.Background(), os.Stdout, proposalStore, policyStore, proposalID)
}

// proposeCommand is propose's testable core. It receives an
// store.ProposalStore and nothing else capable of writing anywhere — no
// policyWriter parameter exists in this function's signature. Combined with
// policy.ProposePolicy itself taking no store handle at all, there are two
// independent, structural reasons a call to propose can never result in a
// write to the real policies table: ProposePolicy can't reach a database,
// and even the code that calls it has nothing to write a policy with.
func proposeCommand(
	ctx context.Context,
	out io.Writer,
	llm policy.LLMClient,
	proposalStore store.ProposalStore,
	policyID, agentID, text string,
) error {
	proposed, err := policy.ProposePolicy(ctx, text, llm)
	if err != nil {
		return err
	}
	proposed.Policy.ID = policyID
	proposed.Policy.AgentID = agentID

	proposalID, err := generateProposalID()
	if err != nil {
		return err
	}

	now := time.Now()
	sp := store.StoredProposal{
		ID:                proposalID,
		Policy:            proposed.Policy,
		Echo:              proposed.Echo,
		RawText:           text,
		CreatedAt:         now,
		ProposalExpiresAt: now.Add(store.ProposalTTL),
	}
	if err := proposalStore.SaveProposal(ctx, sp); err != nil {
		return err
	}

	fmt.Fprintln(out, proposed.Echo)
	fmt.Fprintf(
		out,
		"\nProposal ID: %s (expires in %s — not yet activated)\n",
		proposalID,
		store.ProposalTTL,
	)
	fmt.Fprintf(out, "Run `mandate-cli confirm %s` to activate this policy.\n", proposalID)
	return nil
}

// confirmCommand is confirm's testable core, and the only function in this
// entire codebase that calls SavePolicy. It requires a real, unexpired,
// unconsumed proposal — GetProposal's three distinct error cases
// (not-found, expired, already-consumed) all short-circuit before this
// function ever calls SavePolicy. There is no direct-text path: confirm
// takes a proposal ID, never free text, so there is no way to skip propose
// and land here with unvalidated input.
func confirmCommand(
	ctx context.Context,
	out io.Writer,
	proposalStore store.ProposalStore,
	writer policyWriter,
	proposalID string,
) error {
	sp, err := proposalStore.GetProposal(ctx, proposalID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrProposalNotFound):
			return fmt.Errorf("no proposal found with ID %q: %w", proposalID, err)
		case errors.Is(err, store.ErrProposalExpired):
			return fmt.Errorf("proposal %q expired — run propose again: %w", proposalID, err)
		case errors.Is(err, store.ErrProposalAlreadyConsumed):
			return fmt.Errorf("proposal %q was already confirmed: %w", proposalID, err)
		default:
			return err
		}
	}

	// agent_id is checked separately from ValidateForActivation, not folded
	// into it: ValidateForActivation is also called inside
	// policy.ProposePolicy, before the CLI has assigned AgentID at all — a
	// check here would make every successful propose fail against its own
	// internal validation. This is a defense-in-depth check on top of
	// runPropose's own argument validation, the same "re-check immediately
	// before writing, don't trust an already-stored row" discipline
	// ValidateForActivation itself documents.
	if strings.TrimSpace(sp.Policy.AgentID) == "" {
		return fmt.Errorf("stored proposal has no agent_id, refusing to activate")
	}

	// Defense in depth: re-validate before writing, rather than trusting a
	// row that's already sitting in Postgres was correct the first time.
	if err := policy.ValidateForActivation(sp.Policy); err != nil {
		return fmt.Errorf("stored proposal failed re-validation, refusing to activate: %w", err)
	}

	if err := writer.SavePolicy(ctx, sp.Policy); err != nil {
		return fmt.Errorf("failed to activate policy: %w", err)
	}

	if err := proposalStore.MarkConsumed(ctx, proposalID); err != nil {
		// The policy write already succeeded; failing to mark the proposal
		// consumed only risks a redundant re-confirmation of the same
		// values later, not an unvalidated write — worth surfacing, not
		// worth failing the command over.
		fmt.Fprintf(
			out,
			"warning: policy activated, but failed to mark proposal consumed: %v\n",
			err,
		)
	}

	fmt.Fprintf(out, "Policy %q activated.\n", sp.Policy.ID)
	fmt.Fprintln(out, sp.Echo)
	return nil
}

func generateProposalID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate proposal id: %w", err)
	}
	return "prop_" + hex.EncodeToString(b), nil
}
