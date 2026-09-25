package lark

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Access is open: an unbound or non-member Feishu sender talks to the agent as
// the installer instead of receiving the binding prompt.
func TestFeishuIdentityResolverOpenAccessDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is required for the Feishu identity integration test")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	asUUID := func(s string) pgtype.UUID {
		var u pgtype.UUID
		if err := u.Scan(s); err != nil {
			t.Fatal(err)
		}
		return u
	}

	fx := dbfx.New(pool, "", "")
	suffix := uuid.NewString()
	installerID := fx.User(t, "Installer", "installer-"+suffix+"@multica.test")
	fx.UserID = installerID
	boundID := fx.User(t, "Bound member", "bound-"+suffix+"@multica.test")
	formerID := fx.User(t, "Former member", "former-"+suffix+"@multica.test")
	fx.WorkspaceID = fx.Workspace(t, "Identity workspace", "identity-"+suffix)
	fx.Member(t, fx.WorkspaceID, installerID, "owner")
	fx.Member(t, fx.WorkspaceID, boundID, "member")
	agentID := fx.Agent(t, "Identity agent", "")
	installationID := fx.Insert(t, "channel_installation", dbfx.Cols{
		"workspace_id": fx.WorkspaceID, "agent_id": agentID, "channel_type": channelTypeFeishu,
		"config": dbfx.Raw("'{}'::jsonb"), "status": "active", "installer_user_id": installerID,
	})
	fx.Cleanup(t, `DELETE FROM channel_user_binding WHERE installation_id = $1`, installationID)
	for openID, userID := range map[string]string{"ou_bound": boundID, "ou_former": formerID} {
		fx.Exec(t, `INSERT INTO channel_user_binding (workspace_id, multica_user_id, installation_id, channel_type, channel_user_id)
			VALUES ($1, $2, $3, $4, $5)`, fx.WorkspaceID, userID, installationID, channelTypeFeishu, openID)
	}

	r := &feishuIdentityResolver{store: NewChannelStore(db.New(pool))}
	inst := engine.ResolvedInstallation{
		ID:              asUUID(installationID),
		WorkspaceID:     asUUID(fx.WorkspaceID),
		AgentID:         asUUID(agentID),
		InstallerUserID: asUUID(installerID),
		Active:          true,
	}
	resolve := func(inst engine.ResolvedInstallation, openID string) (engine.ResolvedIdentity, error) {
		return r.ResolveSender(ctx, inst, channel.InboundMessage{Source: channel.Source{SenderID: openID}})
	}

	for _, tc := range []struct {
		name, openID, want string
	}{
		{name: "bound member keeps own identity", openID: "ou_bound", want: boundID},
		{name: "unbound sender acts as installer", openID: "ou_stranger", want: installerID},
		{name: "bound non-member acts as installer", openID: "ou_former", want: installerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolve(inst, tc.openID)
			if err != nil {
				t.Fatalf("ResolveSender: %v", err)
			}
			if got.UserID != asUUID(tc.want) {
				t.Fatalf("UserID = %v, want %s", got.UserID, tc.want)
			}
		})
	}

	t.Run("installer outside workspace still needs binding", func(t *testing.T) {
		orphaned := inst
		orphaned.InstallerUserID = asUUID(formerID)
		if _, err := resolve(orphaned, "ou_stranger"); !errors.Is(err, engine.ErrSenderUnbound) {
			t.Fatalf("err = %v, want ErrSenderUnbound", err)
		}
	})
}
