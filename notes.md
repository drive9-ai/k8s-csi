# Notes

## Drive9 API Assumptions

Based on `github.com/mem9-ai/drive9`:

- API auth uses `Authorization: Bearer <apiKey>`.
- File API is under `/v1/fs/<path>`.
- Directory creation uses `POST /v1/fs/<path>?mkdir`.
- File write uses `PUT /v1/fs/<path>`.
- File read uses `GET /v1/fs/<path>`.
- Stat uses `HEAD /v1/fs/<path>`.
- Delete uses `DELETE /v1/fs/<path>?recursive=1`.

## Kubernetes Integration Choice

CSI-lite is the recommended customer path because it lets application pods consume normal PVCs. The sidecar example is a compatibility fallback when a customer cannot install a CSI driver.
