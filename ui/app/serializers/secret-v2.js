import { EmbeddedRecordsMixin } from '@ember-data/serializer/rest';
import ApplicationSerializer from './application';

export default ApplicationSerializer.extend(EmbeddedRecordsMixin, {
  attrs: {
    versions: { embedded: 'always' },
  },
  secretDataPath: 'data',
  normalizeItems(payload, requestType) {
    if (payload.data.keys && Array.isArray(payload.data.keys)) {
      // if we have data.keys, it's a list of ids, so we map over that
      // and create objects with id's
      return payload.data.keys.map(secret => {
        // secrets don't have an id in the response, so we need to concat the full
        // path of the secret here - the id in the payload is added
        // in the adapter after making the request
        let fullSecretPath = payload.id ? payload.id + secret : secret;

        // if there is no path, it's a "top level" secret, so add
        // a unicode space for the id
        // https://github.com/hashicorp/vault/issues/3348
        if (!fullSecretPath) {
          fullSecretPath = '\u0020';
        }
        return {
          id: fullSecretPath,
          engine_id: payload.backend,
        };
      });
    }
    // transform versions to an array with composite IDs
    if (payload.data.versions) {
      payload.data.versions = Object.keys(payload.data.versions).map(version => {
        let body = payload.data.versions[version];
        body.version = version;
        body.id = JSON.stringify([payload.backend, payload.id, version]);
        return body;
      });
    }
    console.log(
      payload.id,
      "I would honestly expect to this to come back as concatenated, but maybe that's the issue with secret-edit on save"
    );
    payload.path = payload.id; // ARG TODO: set path to id
    payload.id = `${payload.backend}-${payload.id}`; // ARG TODO: this is how you set the id on secret-v2 model
    payload.data.path = payload.path;
    payload.data.id = payload.id;

    payload.data.engine_id = payload.backend;
    return requestType === 'queryRecord' ? payload.data : [payload.data];
  },
  serializeHasMany(snapshot, json, relationship) {
    let newJson = { ...json };
    delete newJson.casRequired;
    this._super(snapshot, newJson, relationship);
  },
});
