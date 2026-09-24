"""The callback for the LiteLLM proxy's config, built from the environment when the proxy loads it:

    litellm_settings:
      callbacks: agentpulse.litellm_proxy.handler

Reads the four `AP_*` settings `agentpulse.connect` reads, and `AP_SERVICE` for the name the proxy records as, `litellm-proxy` by default. A missing setting stops the proxy at startup, where a person is watching, rather than every record being refused quietly. The handler is governed, so a budget can refuse a call before it is forwarded; a proxy with no budgets configured is answered ALLOW and forwards everything.
"""

import os

from . import connect

handler = connect(service=os.environ.get("AP_SERVICE", "").strip() or "litellm-proxy", billed_by="").governed().litellm_callback()
