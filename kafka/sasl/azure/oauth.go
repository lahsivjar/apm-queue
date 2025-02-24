// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package saslazure wraps the creation of a OAUTH sasl.Mechanism
package saslazure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/oauth"
)

// NewFromCredential creates a new OAUTH sasl.Mechanism from an azidentity.
// AzureCredential.
//
// cred, err := azidentity.NewDefaultAzureCredential(nil)
// if err != nil {
// // Handle error
// }
// saslazure.NewFromCredential(cred)
func NewFromCredential(cred azcore.TokenCredential, ns string) sasl.Mechanism {
	scope := fmt.Sprintf("https://%s/.default", ns)
	return oauth.Oauth(func(ctx context.Context) (oauth.Auth, error) {
		token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{scope},
		})
		if err != nil {
			return oauth.Auth{}, err
		}
		return oauth.Auth{Token: token.Token}, nil
	})
}
