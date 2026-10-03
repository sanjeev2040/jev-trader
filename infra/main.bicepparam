// Template for `az deployment group create`. Copy it to main.local.bicepparam
// (git-ignored), replace every REPLACE_ME, and deploy that copy. See
// deploy/README.md. Nothing secret belongs here.
using './main.bicep'

param location = 'centralindia'

// The contents of your SSH public key file, for example ~/.ssh/id_ed25519.pub.
param adminSshPublicKey = 'REPLACE_ME ssh-ed25519 AAAA...'

// Your public IP as a /32 (curl -s https://ifconfig.me). The only address that can reach SSH.
param operatorCidr = 'REPLACE_ME/32'

param githubRepo = 'sanjeev2040/jev-trader'
// The OIDC subject carries numeric IDs: gh api users/OWNER --jq .id ; gh api repos/OWNER/REPO --jq .id
param githubOwnerId = '22476366'
param githubRepoId = '1401087829'

// The pinned Jev model id, and the instruments to paper-trade. With screening on
// (the default) the VM builds each day's list of mid and small caps before the
// open and `watchlist` is only the fallback, used when the screen produced no
// usable file.
param watchlist = 'NSE:INFY,NSE:TCS'
param jevModel = 'jev-1.13.0'
param screenEnabled = true
// Extra flags for `jevtrader screen`, for example '--top 6 --w-news 0.5'.
param screenArgs = ''

param alertEmail = 'REPLACE_ME@example.com'
param budgetAmount = 25
param budgetStartDate = '2026-10-01'

// Optional: your Entra object id (az ad signed-in-user show --query id -o tsv),
// so you can enter the Key Vault secrets. Leave empty to grant the role yourself.
param operatorObjectId = ''
