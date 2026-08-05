# Why `WgGatewayClient.Spec.Endpoints` was introduced

## Before this PR

A gateway was always a single Deployment. The remote server exposed a single Service, so the client only ever needed **one** WireGuard endpoint.

That single endpoint was stored in `GatewayClient.Spec.Endpoint` (singular) and rendered directly into the `WgGatewayClient` deployment template. The controller created one Deployment from the template and the pod used that endpoint. No per-replica decision making was needed.

## Why `Endpoints` (plural) is now needed

With multi-replica support:

- The server creates **one Service per replica**, so there are N server endpoints.
- The client creates **one Deployment per replica**, so there are N client pods.
- Each client pod must connect to a **different** server endpoint to spread the load.

A single `Endpoint` field can only hold one address/port pair, so it cannot express the N server endpoints required for N replicas. Therefore `GatewayClient.Spec.Endpoints` and `WgGatewayClient.Spec.Endpoints` were added as slices of `EndpointStatus`.

## How the endpoint reaches each client pod

1. `liqoctl peer` collects all server endpoints from `GatewayServer.Status.Endpoints` and writes them into `GatewayClient.Spec.Endpoints`.
2. The client-operator copies that list into `WgGatewayClient.Spec.Endpoints`.
3. The `WgGatewayClient` controller, when creating/updating each per-replica Deployment, picks the i-th endpoint and injects `--endpoint-address`/`--endpoint-ports` into the `wireguard` container of replica i.

## Is `Endpoints` mandatory?

At the CRD level both `Endpoint` (singular, deprecated) and `Endpoints` (plural) are `omitempty`, so neither is strictly required by OpenAPI.

Functionally, however:

- For `replicas == 1`, the old `GatewayClient.Spec.Endpoint` still works for backward compatibility. The controller will also accept `Endpoints` with one element.
- For `replicas > 1`, `Endpoints` is effectively required. Without it the controller cannot assign distinct server endpoints to the client pods and extra replicas would have to fall back to a shared endpoint.

## Note on the client template

The current Helm template for the WireGuard client still references the deprecated `.Spec.Endpoint` (singular) when rendering the initial deployment template. This is a backward-compatibility leftover. The `WgGatewayClient` controller overrides those args for every replica using `WgGatewayClient.Spec.Endpoints`, so the rendered value is replaced before the pod starts. A future cleanup could remove the endpoint args from the template entirely and rely solely on controller injection.
