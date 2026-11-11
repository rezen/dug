# Update center root certificates

The two certificates Jenkins core trusts to sign the update center, copied from
`war/src/main/webapp/WEB-INF/update-center-rootCAs/` in
<https://github.com/jenkinsci/jenkins> and confirmed byte-identical to the copies
inside the released `jenkins.war` (2.590) on 2026-11-11. `signature.go` embeds
them; a test pins their SHA-256 fingerprints.

| File | Subject | Valid until | SHA-256 fingerprint |
| --- | --- | --- | --- |
| `jenkins-update-center-root-ca-2.pem` | CN=Kohsuke Kawaguchi | 2028-04-05 | `2E:D6:FE:80:E5:7A:F1:6A:2A:26:26:E8:A8:23:5A:6D:13:42:E4:8C:DF:63:4A:1C:FA:BC:E9:34:15:45:E5:B9` |
| `jenkins-update-center-root-ca-2026.pem` | OU=Jenkins Infrastructure Team | 2036-07-24 | `49:22:70:D3:3F:4D:FB:F1:12:28:57:F9:43:40:58:F5:2A:5A:11:99:3D:BC:D5:D0:13:FE:01:7C:E6:57:EE:7C` |

The current signing certificate chains to the first. Replace a file only after
checking the new certificate against the same upstream directory.
