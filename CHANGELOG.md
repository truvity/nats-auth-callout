# Changelog

What changed for a consumer, per version, newest first. A version with no
heading here is a patch cut automatically for dependency bumps alone; its
GitHub Release lists them. The chart and the image are released together
at every version.

## v1.0.1

- **`values.schema.json` admits the chart's own `natsURL: ""`
  placeholder.** v1.0.0's rule accepted only a non-empty `nats://` URL,
  so `helm lint` on a clean checkout failed on the shipped default. Empty
  now passes the schema and is still refused by the template's
  `required`, so an install without a real `natsURL` fails exactly as
  before; anything non-empty must still be a `nats://` URL. A values file
  that installed before installs unchanged.

## v1.0.0

- First release: the `nats-auth-callout` chart and the responder image,
  with the strict `values.schema.json`, the egress NetworkPolicy (off by
  default), TokenReview retry and the decision cache, and `/readyz`
  exercising the real dependency chain.
