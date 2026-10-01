// Serves the static homepage and sends www.flopwire.com to the apex domain.
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.hostname === "www.flopwire.com") {
      url.hostname = "flopwire.com";
      return Response.redirect(url.toString(), 301);
    }
    return env.ASSETS.fetch(request);
  },
};
