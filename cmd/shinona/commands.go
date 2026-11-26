package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/term"

	"github.com/OXeu/shinona/internal/auth"
)

func execute(ctx context.Context, logger *slog.Logger, args []string) error {
	environment, err := readEnvironment(".env")
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return serve(ctx, logger, environment)
	}
	if len(args) != 2 || (args[0] != "create-user" && args[0] != "adopt-legacy" && args[0] != "grant-admin" && args[0] != "revoke-admin") {
		return errors.New("usage: shinona [create-user EMAIL | adopt-legacy USER_ID | grant-admin USER_ID | revoke-admin USER_ID]")
	}

	repository, err := openDatabase(ctx, environment)
	if err != nil {
		return err
	}
	defer func() {
		if err := repository.Close(); err != nil {
			logger.Error("close SQLite database", "error", err)
		}
	}()

	switch args[0] {
	case "create-user":
		value, err := readPassword()
		if err != nil {
			return err
		}
		hasher, err := passwordHasher(environment)
		if err != nil {
			return err
		}
		service := auth.New(repository.Accounts, hasher, uuid.NewString)
		user, err := service.CreatePasswordUser(ctx, args[1], value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "Created %s (%s)\n", user.Email, user.ID)
		return err
	case "grant-admin", "revoke-admin":
		return repository.Accounts.SetAdmin(ctx, args[1], args[0] == "grant-admin")
	default:
		count, err := repository.Conversations.AdoptLegacy(ctx, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "Imported %d legacy runs.\n", count)
		return err
	}
}

func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		_, _ = fmt.Fprint(os.Stderr, "Password: ")
		value, err := term.ReadPassword(fd)
		_, _ = fmt.Fprintln(os.Stderr)
		return string(value), err
	}
	const lineEndingAndOverflow = len("\r\n") + 1
	reader := bufio.NewReader(io.LimitReader(os.Stdin, int64(auth.MaxPasswordBytes+lineEndingAndOverflow)))
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}
