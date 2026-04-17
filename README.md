# gh-audit

GitHub audit tool — syncs commit, PR, review, and check data for compliance auditing.

## Documentation

- [Domain Model & UML Diagram](internal/model/README.md)

## Future Enhancements

- **Webhook-based sync**: After the initial historical sync, use GitHub `push` event webhooks for real-time updates. Zero API cost per new commit, replacing polling entirely for steady-state operation.
