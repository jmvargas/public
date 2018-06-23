package cli

import (
	"fmt"

	"cirello.io/snippetsd/pkg/errors"
	"cirello.io/snippetsd/pkg/infra/repositories"
	"cirello.io/snippetsd/pkg/models/user"
	"gopkg.in/urfave/cli.v1"
)

func (c *commands) addUser() cli.Command {
	return cli.Command{
		Name:  "add",
		Usage: "add a user",
		Action: func(ctx *cli.Context) error {
			u, err := user.NewFromEmail(ctx.Args().Get(0), ctx.Args().Get(1), ctx.Args().Get(2))
			if err != nil {
				return errors.E(ctx, err, "cannot create user from email")
			}

			if _, err := repositories.Users(c.db).Insert(u); err != nil {
				return errors.E(ctx, err, "cannot store the new user")
			}

			fmt.Println(u, "added")
			return nil
		},
	}
}
