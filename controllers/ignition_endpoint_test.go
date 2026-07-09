/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("ignition endpoint URL helpers", func() {
	It("normalizes unbracketed IPv6 URLs", func() {
		Expect(normalizeHTTPURL("https://fd2e:6f44:5dd8:c956::14:31187")).
			To(Equal("https://[fd2e:6f44:5dd8:c956::14]:31187"))
		Expect(normalizeHTTPURL("https://fd2e:6f44:5dd8:c956::14:31187/ignition")).
			To(Equal("https://[fd2e:6f44:5dd8:c956::14]:31187/ignition"))
	})

	It("leaves IPv4 and bracketed IPv6 URLs unchanged", func() {
		Expect(normalizeHTTPURL("https://1.2.3.4:555/ignition")).To(Equal("https://1.2.3.4:555/ignition"))
		Expect(normalizeHTTPURL("http://[1080::8:800:200c:417a]:123/config")).
			To(Equal("http://[1080::8:800:200c:417a]:123/config"))
	})

	It("prepares ignition endpoint URLs for AgentClusterInstall", func() {
		Expect(ignitionEndpointURLForACI("https://fd2e:6f44:5dd8:c956::14:31187/ignition")).
			To(Equal("https://[fd2e:6f44:5dd8:c956::14]:31187"))
		Expect(ignitionEndpointURLForACI("https://1.2.3.4:555/ignition")).
			To(Equal("https://1.2.3.4:555"))
		Expect(ignitionEndpointURLForACI("https://1.2.3.4:555")).
			To(Equal("https://1.2.3.4:555"))
		Expect(ignitionEndpointURLForACI("")).To(Equal(""))
	})
})
