// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net/http"

	"github.com/alecthomas/kong"
	"github.com/pkg/errors"
	"github.com/pterm/pterm"

	"github.com/upbound/up-sdk-go"

	"github.com/upbound/up/internal/upbound"
)

const (
	logoutPath = "/v1/logout"

	errLogoutFailed      = "unable to logout"
	errRemoveTokenFailed = "failed to remove token"
)

// AfterApply sets default values in login after assignment and validation.
func (c *logoutCmd) AfterApply(kongCtx *kong.Context) error {
	upCtx, err := upbound.NewFromFlags(c.Flags)
	if err != nil {
		return err
	}
	kongCtx.Bind(upCtx)
	cfg, err := upCtx.BuildSDKConfig(upCtx.Profile.Session)
	if err != nil {
		return err
	}
	c.client = cfg.Client
	return nil
}

// logoutCmd invalidates a stored session token for a given profile.
type logoutCmd struct {
	client up.Client

	// Common Upbound API configuration
	Flags upbound.Flags `embed:""`
}

// Run executes the logout command.
func (c *logoutCmd) Run(p pterm.TextPrinter, upCtx *upbound.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	req, err := c.client.NewRequest(ctx, http.MethodPost, logoutPath, "", nil)
	if err != nil {
		return errors.Wrap(err, errLogoutFailed)
	}
	if err := c.client.Do(req, nil); err != nil {
		return errors.Wrap(err, errLogoutFailed)
	}
	// Logout is successful, remove token from config and update.
	upCtx.Profile.Session = ""
	if err := upCtx.Cfg.AddOrUpdateUpboundProfile(upCtx.ProfileName, upCtx.Profile); err != nil {
		return errors.Wrap(err, errRemoveTokenFailed)
	}
	if err := upCtx.CfgSrc.UpdateConfig(upCtx.Cfg); err != nil {
		return errors.Wrap(err, errUpdateConfig)
	}

	p.Printfln("%s logged out", upCtx.Profile.ID)
	return nil
}
